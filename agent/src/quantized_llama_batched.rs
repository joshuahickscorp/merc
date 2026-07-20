#![allow(clippy::all, dead_code)]

use std::collections::{HashMap, VecDeque};

use candle_core::quantized::gguf_file;
use candle_core::quantized::QTensor;
use candle_core::{DType, Device, IndexOp, Result, Tensor};
use candle_nn::{Embedding, Module};
use candle_transformers::quantized_nn::RmsNorm;

pub const MAX_SEQ_LEN: usize = 8192;

const MASK_CACHE_CAP: usize = 64;

#[derive(Debug, Clone)]
struct KvCacheSlot {
    buf: Option<Tensor>,
    cur_len: usize,
    cap: usize,
}

impl KvCacheSlot {
    fn new() -> Self {
        Self {
            buf: None,
            cur_len: 0,
            cap: MAX_SEQ_LEN,
        }
    }

    fn reset(&mut self) {
        self.buf = None;
        self.cur_len = 0;
    }

    fn reset_with_cap(&mut self, cap: usize) {
        self.buf = None;
        self.cur_len = 0;
        self.cap = cap.clamp(1, MAX_SEQ_LEN);
    }

    fn truncate(&mut self, len: usize) -> Result<()> {
        if len > self.cur_len {
            candle_core::bail!(
                "kv truncate cannot grow live length from {} to {len}",
                self.cur_len
            );
        }
        self.cur_len = len;
        Ok(())
    }

    fn append(&mut self, src: &Tensor) -> Result<Tensor> {
        let (b_sz, n_kv_head, seq_len, head_dim) = src.dims4()?;
        if self.buf.is_none() {
            let alloc_len = self.cap.max(self.cur_len + seq_len).min(MAX_SEQ_LEN);
            let buf = Tensor::zeros(
                (b_sz, n_kv_head, alloc_len, head_dim),
                src.dtype(),
                src.device(),
            )?;
            self.buf = Some(buf);
            self.cur_len = 0;
        }
        let mut buf = self.buf.as_ref().unwrap().clone();
        if self.cur_len + seq_len > buf.dim(2)? {
            let grown_len = (self.cur_len + seq_len).min(MAX_SEQ_LEN);
            let grown = Tensor::zeros(
                (b_sz, n_kv_head, grown_len, head_dim),
                src.dtype(),
                src.device(),
            )?;
            if self.cur_len > 0 {
                let live = buf.narrow(2, 0, self.cur_len)?.contiguous()?;
                grown.slice_set(&live, 2, 0)?;
            }
            buf = grown;
            self.buf = Some(buf.clone());
        }
        let buf = self.buf.as_mut().unwrap();
        buf.slice_set(&src.contiguous()?, 2, self.cur_len)?;
        self.cur_len += seq_len;
        buf.narrow(2, 0, self.cur_len)?.contiguous()
    }

    fn compact(&mut self, idx: &Tensor) -> Result<()> {
        if let Some(buf) = &self.buf {
            let live = buf.narrow(2, 0, self.cur_len)?.contiguous()?;
            let kept = live.index_select(idx, 0)?;
            let (b_sz, n_kv_head, _seq, head_dim) = kept.dims4()?;
            let alloc_len = self.cap.max(self.cur_len).min(MAX_SEQ_LEN);
            let new_buf = Tensor::zeros(
                (b_sz, n_kv_head, alloc_len, head_dim),
                kept.dtype(),
                kept.device(),
            )?;
            new_buf.slice_set(&kept.contiguous()?, 2, 0)?;
            self.buf = Some(new_buf);
        }
        Ok(())
    }

    fn snapshot(&self) -> Result<Option<(Tensor, usize)>> {
        match &self.buf {
            Some(buf) if self.cur_len > 0 => {
                let live = buf.narrow(2, 0, self.cur_len)?.contiguous()?;
                Ok(Some((live, self.cur_len)))
            }
            _ => Ok(None),
        }
    }

    fn restore(&mut self, snapshot: &(Tensor, usize)) -> Result<()> {
        let (live, len) = snapshot;
        let (b_sz, n_kv_head, _seq, head_dim) = live.dims4()?;
        let alloc_len = self.cap.max(*len).min(MAX_SEQ_LEN);
        let new_buf = Tensor::zeros(
            (b_sz, n_kv_head, alloc_len, head_dim),
            live.dtype(),
            live.device(),
        )?;
        new_buf.slice_set(&live.contiguous()?, 2, 0)?;
        self.buf = Some(new_buf);
        self.cur_len = *len;
        Ok(())
    }

    fn restore_broadcast(&mut self, snapshot: &(Tensor, usize), b_sz: usize) -> Result<()> {
        let (live, len) = snapshot;
        let (src_b, n_kv_head, _seq, head_dim) = live.dims4()?;
        if src_b != 1 {
            candle_core::bail!("restore_broadcast: snapshot batch dim must be 1, got {src_b}");
        }
        let live_b = live.repeat((b_sz, 1, 1, 1))?;
        let alloc_len = self.cap.max(*len).min(MAX_SEQ_LEN);
        let new_buf = Tensor::zeros(
            (b_sz, n_kv_head, alloc_len, head_dim),
            live_b.dtype(),
            live_b.device(),
        )?;
        new_buf.slice_set(&live_b.contiguous()?, 2, 0)?;
        self.buf = Some(new_buf);
        self.cur_len = *len;
        Ok(())
    }
}

#[derive(Debug, Clone)]
pub struct KvSnapshot {
    k: Option<(Tensor, usize)>,
    v: Option<(Tensor, usize)>,
}

#[derive(Debug, Clone)]
struct QMatMul {
    inner: candle_core::quantized::QMatMul,
    span: tracing::Span,
}

impl QMatMul {
    fn from_qtensor(qtensor: QTensor) -> Result<Self> {
        let inner = candle_core::quantized::QMatMul::from_qtensor(qtensor)?;
        let span = tracing::span!(tracing::Level::TRACE, "qmatmul");
        Ok(Self { inner, span })
    }

    fn forward(&self, xs: &Tensor) -> Result<Tensor> {
        let _enter = self.span.enter();
        self.inner.forward(xs)
    }
}

#[derive(Debug, Clone)]
struct Mlp {
    feed_forward_w1: QMatMul,
    feed_forward_w2: QMatMul,
    feed_forward_w3: QMatMul,
}

impl Module for Mlp {
    fn forward(&self, xs: &Tensor) -> Result<Tensor> {
        let w1 = self.feed_forward_w1.forward(xs)?;
        let w3 = self.feed_forward_w3.forward(xs)?;
        self.feed_forward_w2
            .forward(&(candle_nn::ops::silu(&w1)? * w3)?)
    }
}

#[derive(Debug, Clone)]
struct LayerWeights {
    attention_wq: QMatMul,
    attention_wk: QMatMul,
    attention_wv: QMatMul,
    attention_wo: QMatMul,
    attention_norm: RmsNorm,
    mlp: Mlp,
    ffn_norm: RmsNorm,
    n_head: usize,
    n_kv_head: usize,
    head_dim: usize,
    cos: Tensor,
    sin: Tensor,
    neg_inf: Tensor,
    kv_k: KvCacheSlot,
    kv_v: KvCacheSlot,
    span_attn: tracing::Span,
    span_rot: tracing::Span,
    span_mlp: tracing::Span,
}

fn masked_fill(on_false: &Tensor, mask: &Tensor, on_true: &Tensor) -> Result<Tensor> {
    let shape = mask.shape();
    let m = mask.where_cond(&on_true.broadcast_as(shape.dims())?, on_false)?;
    Ok(m)
}

#[allow(clippy::needless_range_loop)]
pub fn build_padded_mask(
    real_len: &[usize],
    q_global_pos: &[Vec<usize>],
    cached_len: usize,
    seq_len: usize,
    device: &Device,
) -> Result<Tensor> {
    let b_sz = real_len.len();
    let kv_len = cached_len + seq_len;
    let mut data: Vec<u8> = Vec::with_capacity(b_sz * seq_len * kv_len);
    for r in 0..b_sz {
        for i in 0..seq_len {
            let qpos = q_global_pos[r][i];
            for j in 0..kv_len {
                let forbidden = if j < cached_len {
                    j >= real_len[r]
                } else {
                    q_global_pos[r][j - cached_len] > qpos
                };
                data.push(u8::from(forbidden));
            }
        }
    }
    Tensor::from_vec(data, (b_sz, 1, seq_len, kv_len), device)
}

impl LayerWeights {
    fn apply_rotary_emb(&self, x: &Tensor, index_pos: usize) -> Result<Tensor> {
        let _enter = self.span_rot.enter();
        let (_b_sz, _n_head, seq_len, _n_embd) = x.dims4()?;
        let cos = self.cos.narrow(0, index_pos, seq_len)?;
        let sin = self.sin.narrow(0, index_pos, seq_len)?;
        let x = x.contiguous()?;
        candle_nn::rotary_emb::rope_i(&x, &cos, &sin)
    }

    fn apply_rotary_emb_per_row(&self, x: &Tensor, cos3: &Tensor, sin3: &Tensor) -> Result<Tensor> {
        let _enter = self.span_rot.enter();
        let x = x.contiguous()?;
        candle_nn::rotary_emb::rope_i(&x, cos3, sin3)
    }

    fn forward_attn(
        &mut self,
        x: &Tensor,
        mask: Option<&Tensor>,
        index_pos: usize,
        seq_cap: usize,
    ) -> Result<Tensor> {
        let _enter = self.span_attn.enter();
        let (b_sz, seq_len, n_embd) = x.dims3()?;
        let q = self.attention_wq.forward(x)?;
        let k = self.attention_wk.forward(x)?;
        let v = self.attention_wv.forward(x)?;

        let q = q
            .reshape((b_sz, seq_len, self.n_head, self.head_dim))?
            .transpose(1, 2)?;
        let k = k
            .reshape((b_sz, seq_len, self.n_kv_head, self.head_dim))?
            .transpose(1, 2)?;
        let v = v
            .reshape((b_sz, seq_len, self.n_kv_head, self.head_dim))?
            .transpose(1, 2)?
            .contiguous()?;

        let q = self.apply_rotary_emb(&q, index_pos)?;
        let k = self.apply_rotary_emb(&k, index_pos)?;

        if index_pos == 0 {
            self.kv_k.reset_with_cap(seq_cap);
            self.kv_v.reset_with_cap(seq_cap);
        }
        let k = self.kv_k.append(&k)?;
        let v = self.kv_v.append(&v)?;

        let y = if q.device().is_metal() && seq_len == 1 {
            candle_nn::ops::sdpa(
                &q,
                &k,
                &v,
                None,
                false,
                1. / (self.head_dim as f32).sqrt(),
                1.,
            )?
        } else {
            let k = candle_transformers::utils::repeat_kv(k, self.n_head / self.n_kv_head)?;
            let v = candle_transformers::utils::repeat_kv(v, self.n_head / self.n_kv_head)?;

            let att = (q.matmul(&k.t()?)? / (self.head_dim as f64).sqrt())?;
            let att = match mask {
                None => att,
                Some(mask) => {
                    let mask = mask.broadcast_as(att.shape())?;
                    masked_fill(&att, &mask, &self.neg_inf)?
                }
            };
            let att = candle_nn::ops::softmax_last_dim(&att)?;
            att.matmul(&v.contiguous()?)?
        };

        let y = y.transpose(1, 2)?.reshape(&[b_sz, seq_len, n_embd])?;
        let y = self.attention_wo.forward(&y)?;
        Ok(y)
    }

    #[allow(clippy::too_many_arguments)]
    fn forward_attn_per_row(
        &mut self,
        x: &Tensor,
        mask: &Tensor,
        cos3: &Tensor,
        sin3: &Tensor,
        seq_cap: usize,
        fresh: bool,
    ) -> Result<Tensor> {
        let _enter = self.span_attn.enter();
        let (b_sz, seq_len, n_embd) = x.dims3()?;
        let q = self.attention_wq.forward(x)?;
        let k = self.attention_wk.forward(x)?;
        let v = self.attention_wv.forward(x)?;

        let q = q
            .reshape((b_sz, seq_len, self.n_head, self.head_dim))?
            .transpose(1, 2)?;
        let k = k
            .reshape((b_sz, seq_len, self.n_kv_head, self.head_dim))?
            .transpose(1, 2)?;
        let v = v
            .reshape((b_sz, seq_len, self.n_kv_head, self.head_dim))?
            .transpose(1, 2)?
            .contiguous()?;

        let q = self.apply_rotary_emb_per_row(&q, cos3, sin3)?;
        let k = self.apply_rotary_emb_per_row(&k, cos3, sin3)?;

        if fresh {
            self.kv_k.reset_with_cap(seq_cap);
            self.kv_v.reset_with_cap(seq_cap);
        }
        let k = self.kv_k.append(&k)?;
        let v = self.kv_v.append(&v)?;

        let k = candle_transformers::utils::repeat_kv(k, self.n_head / self.n_kv_head)?;
        let v = candle_transformers::utils::repeat_kv(v, self.n_head / self.n_kv_head)?;
        let att = (q.matmul(&k.t()?)? / (self.head_dim as f64).sqrt())?;
        let mask = mask.broadcast_as(att.shape())?;
        let att = masked_fill(&att, &mask, &self.neg_inf)?;
        let att = candle_nn::ops::softmax_last_dim(&att)?;
        let y = att.matmul(&v.contiguous()?)?;

        let y = y.transpose(1, 2)?.reshape(&[b_sz, seq_len, n_embd])?;
        let y = self.attention_wo.forward(&y)?;
        Ok(y)
    }
}

#[derive(Debug, Clone)]
pub struct ModelWeights {
    tok_embeddings: Embedding,
    layers: Vec<LayerWeights>,
    norm: RmsNorm,
    output: QMatMul,
    masks: HashMap<(usize, usize), Tensor>,
    mask_order: VecDeque<(usize, usize)>,
    next_seq_cap: Option<usize>,
    span: tracing::Span,
    span_output: tracing::Span,
}

fn precomput_freqs_cis(
    head_dim: usize,
    freq_base: f32,
    device: &Device,
) -> Result<(Tensor, Tensor)> {
    let theta: Vec<_> = (0..head_dim)
        .step_by(2)
        .map(|i| 1f32 / freq_base.powf(i as f32 / head_dim as f32))
        .collect();
    let theta = Tensor::new(theta.as_slice(), device)?;
    let idx_theta = Tensor::arange(0, MAX_SEQ_LEN as u32, device)?
        .to_dtype(DType::F32)?
        .reshape((MAX_SEQ_LEN, 1))?
        .matmul(&theta.reshape((1, theta.elem_count()))?)?;
    let cos = idx_theta.cos()?;
    let sin = idx_theta.sin()?;
    Ok((cos, sin))
}

impl ModelWeights {
    pub fn from_gguf<R: std::io::Seek + std::io::Read>(
        ct: gguf_file::Content,
        reader: &mut R,
        device: &Device,
    ) -> Result<Self> {
        let md_get = |s: &str| match ct.metadata.get(s) {
            None => candle_core::bail!("cannot find {s} in metadata"),
            Some(v) => Ok(v),
        };

        let arch = ct
            .metadata
            .get("general.architecture")
            .and_then(|v| v.to_string().ok())
            .map(|s| s.to_string())
            .unwrap_or_else(|| "llama".to_string());

        if arch != "llama" {
            candle_core::bail!("unsupported GGUF architecture {arch:?}; expected llama")
        }
        let mdk = |k: &str| format!("llama.{k}");
        let head_count = md_get(&mdk("attention.head_count"))?.to_u32()? as usize;
        let head_count_kv = md_get(&mdk("attention.head_count_kv"))?.to_u32()? as usize;
        let block_count = md_get(&mdk("block_count"))?.to_u32()? as usize;
        let embedding_length = md_get(&mdk("embedding_length"))?.to_u32()? as usize;
        let rope_dim = md_get(&mdk("rope.dimension_count"))
            .and_then(|v| v.to_u32())
            .map(|d| d as usize)
            .unwrap_or(embedding_length / head_count);
        let rms_norm_eps = md_get(&mdk("attention.layer_norm_rms_epsilon"))?.to_f32()? as f64;

        let rope_freq_base = md_get(&mdk("rope.freq_base"))
            .and_then(|m| m.to_f32())
            .unwrap_or(10000f32);
        let (cos, sin) = precomput_freqs_cis(rope_dim, rope_freq_base, device)?;
        let neg_inf = Tensor::new(f32::NEG_INFINITY, device)?;

        let tok_embeddings_q = ct.tensor(reader, "token_embd.weight", device)?;
        let tok_embeddings = tok_embeddings_q.dequantize(device)?;
        let norm = RmsNorm::from_qtensor(
            ct.tensor(reader, "output_norm.weight", device)?,
            rms_norm_eps,
        )?;
        let output = match ct.tensor(reader, "output.weight", device) {
            Ok(tensor) => tensor,
            Err(_) => tok_embeddings_q,
        };
        let mut layers = Vec::with_capacity(block_count);
        for layer_idx in 0..block_count {
            let prefix = format!("blk.{layer_idx}");
            let attention_wq = ct.tensor(reader, &format!("{prefix}.attn_q.weight"), device)?;
            let attention_wk = ct.tensor(reader, &format!("{prefix}.attn_k.weight"), device)?;
            let attention_wv = ct.tensor(reader, &format!("{prefix}.attn_v.weight"), device)?;
            let attention_wo =
                ct.tensor(reader, &format!("{prefix}.attn_output.weight"), device)?;
            let mlp = Mlp {
                feed_forward_w1: QMatMul::from_qtensor(ct.tensor(
                    reader,
                    &format!("{prefix}.ffn_gate.weight"),
                    device,
                )?)?,
                feed_forward_w2: QMatMul::from_qtensor(ct.tensor(
                    reader,
                    &format!("{prefix}.ffn_down.weight"),
                    device,
                )?)?,
                feed_forward_w3: QMatMul::from_qtensor(ct.tensor(
                    reader,
                    &format!("{prefix}.ffn_up.weight"),
                    device,
                )?)?,
            };
            let attention_norm =
                ct.tensor(reader, &format!("{prefix}.attn_norm.weight"), device)?;
            let ffn_norm = ct.tensor(reader, &format!("{prefix}.ffn_norm.weight"), device)?;
            let span_attn = tracing::span!(tracing::Level::TRACE, "attn");
            let span_rot = tracing::span!(tracing::Level::TRACE, "attn-rot");
            let span_mlp = tracing::span!(tracing::Level::TRACE, "attn-mlp");
            layers.push(LayerWeights {
                attention_wq: QMatMul::from_qtensor(attention_wq)?,
                attention_wk: QMatMul::from_qtensor(attention_wk)?,
                attention_wv: QMatMul::from_qtensor(attention_wv)?,
                attention_wo: QMatMul::from_qtensor(attention_wo)?,
                attention_norm: RmsNorm::from_qtensor(attention_norm, rms_norm_eps)?,
                mlp,
                ffn_norm: RmsNorm::from_qtensor(ffn_norm, rms_norm_eps)?,
                n_head: head_count,
                n_kv_head: head_count_kv,
                head_dim: embedding_length / head_count,
                cos: cos.clone(),
                sin: sin.clone(),
                neg_inf: neg_inf.clone(),
                kv_k: KvCacheSlot::new(),
                kv_v: KvCacheSlot::new(),
                span_attn,
                span_rot,
                span_mlp,
            })
        }
        let span = tracing::span!(tracing::Level::TRACE, "model");
        let span_output = tracing::span!(tracing::Level::TRACE, "output");
        Ok(Self {
            tok_embeddings: Embedding::new(tok_embeddings, embedding_length),
            layers,
            norm,
            output: QMatMul::from_qtensor(output)?,
            masks: HashMap::new(),
            mask_order: VecDeque::new(),
            next_seq_cap: None,
            span,
            span_output,
        })
    }

    fn mask(&mut self, seq_len: usize, index_pos: usize, device: &Device) -> Result<Tensor> {
        let kv_len = index_pos + seq_len;
        let key = (seq_len, kv_len);
        if let Some(mask) = self.masks.get(&key) {
            Ok(mask.clone())
        } else {
            let mask = candle_transformers::utils::build_causal_mask(seq_len, index_pos, device)?;
            while self.mask_order.len() >= MASK_CACHE_CAP {
                if let Some(old) = self.mask_order.pop_front() {
                    self.masks.remove(&old);
                } else {
                    break;
                }
            }
            self.masks.insert(key, mask.clone());
            self.mask_order.push_back(key);
            Ok(mask)
        }
    }

    pub fn compact_kv_cache(&mut self, keep: &[usize]) -> Result<()> {
        if keep.is_empty() {
            return Ok(());
        }
        let device = self.tok_embeddings.embeddings().device();
        let idx = Tensor::from_vec(
            keep.iter().map(|&i| i as u32).collect::<Vec<u32>>(),
            keep.len(),
            device,
        )?;
        for layer in self.layers.iter_mut() {
            layer.kv_k.compact(&idx)?;
            layer.kv_v.compact(&idx)?;
        }
        Ok(())
    }

    pub fn kv_cache_len(&self) -> Result<usize> {
        let Some(first) = self.layers.first() else {
            return Ok(0);
        };
        let expected = first.kv_k.cur_len;
        for (index, layer) in self.layers.iter().enumerate() {
            if layer.kv_k.cur_len != expected || layer.kv_v.cur_len != expected {
                candle_core::bail!(
                    "incoherent KV lengths at layer {index}: k={}, v={}, expected={expected}",
                    layer.kv_k.cur_len,
                    layer.kv_v.cur_len
                );
            }
        }
        Ok(expected)
    }

    pub fn truncate_kv_cache(&mut self, len: usize) -> Result<()> {
        for (index, layer) in self.layers.iter().enumerate() {
            if layer.kv_k.cur_len < len || layer.kv_v.cur_len < len {
                candle_core::bail!(
                    "kv truncate cannot grow layer {index} to {len}: k={}, v={}",
                    layer.kv_k.cur_len,
                    layer.kv_v.cur_len
                );
            }
        }
        for layer in self.layers.iter_mut() {
            layer.kv_k.truncate(len)?;
            layer.kv_v.truncate(len)?;
        }
        Ok(())
    }

    pub fn snapshot_kv_cache(&self) -> Result<Vec<KvSnapshot>> {
        let mut out = Vec::with_capacity(self.layers.len());
        for layer in self.layers.iter() {
            let k = layer.kv_k.snapshot()?;
            let v = layer.kv_v.snapshot()?;
            out.push(KvSnapshot { k, v });
        }
        Ok(out)
    }

    pub fn restore_kv_cache(&mut self, snapshot: &[KvSnapshot]) -> Result<()> {
        if snapshot.len() != self.layers.len() {
            candle_core::bail!(
                "restore_kv_cache: snapshot has {} layers, model has {}",
                snapshot.len(),
                self.layers.len()
            );
        }
        for (layer, snap) in self.layers.iter_mut().zip(snapshot.iter()) {
            match (&snap.k, &snap.v) {
                (Some(k), Some(v)) => {
                    layer.kv_k.restore(k)?;
                    layer.kv_v.restore(v)?;
                }
                _ => {
                    layer.kv_k.reset();
                    layer.kv_v.reset();
                }
            }
        }
        Ok(())
    }

    pub fn restore_kv_cache_broadcast(
        &mut self,
        snapshot: &[KvSnapshot],
        b_sz: usize,
    ) -> Result<()> {
        if snapshot.len() != self.layers.len() {
            candle_core::bail!(
                "restore_kv_cache_broadcast: snapshot has {} layers, model has {}",
                snapshot.len(),
                self.layers.len()
            );
        }
        for (layer, snap) in self.layers.iter_mut().zip(snapshot.iter()) {
            match (&snap.k, &snap.v) {
                (Some(k), Some(v)) => {
                    layer.kv_k.restore_broadcast(k, b_sz)?;
                    layer.kv_v.restore_broadcast(v, b_sz)?;
                }
                _ => candle_core::bail!(
                    "restore_kv_cache_broadcast: snapshot has no prefix to broadcast from"
                ),
            }
        }
        Ok(())
    }

    pub fn set_next_seq_cap(&mut self, cap: Option<usize>) {
        self.next_seq_cap = cap;
    }

    pub fn kv_bytes_per_token_per_row(&self) -> usize {
        let n_layers = self.layers.len();
        let (n_kv_head, head_dim) = self
            .layers
            .first()
            .map(|l| (l.n_kv_head, l.head_dim))
            .unwrap_or((0, 0));
        2 * n_layers * n_kv_head * head_dim * std::mem::size_of::<f32>()
    }

    pub fn forward(&mut self, x: &Tensor, index_pos: usize) -> Result<Tensor> {
        let (_b_sz, seq_len) = x.dims2()?;
        if index_pos + seq_len > MAX_SEQ_LEN {
            candle_core::bail!(
                "context length exceeded: index_pos={index_pos} + seq_len={seq_len} = {} \
                 tokens, which is beyond this model's MAX_SEQ_LEN={MAX_SEQ_LEN} ceiling  -  \
                 shorten the prompt/max_tokens or use a job type/model with a larger context window",
                index_pos + seq_len
            );
        }
        let mask = if seq_len == 1 {
            None
        } else {
            Some(self.mask(seq_len, index_pos, x.device())?)
        };
        let seq_cap = self.next_seq_cap.take().unwrap_or(MAX_SEQ_LEN);
        let _enter = self.span.enter();
        let mut layer_in = self.tok_embeddings.forward(x)?;
        for layer in self.layers.iter_mut() {
            let x = layer_in;
            let residual = &x;
            let x = layer.attention_norm.forward(&x)?;
            let attn = layer.forward_attn(&x, mask.as_ref(), index_pos, seq_cap)?;
            let x = (attn + residual)?;

            let _enter = layer.span_mlp.enter();
            let residual = &x;
            let x = layer.ffn_norm.forward(&x)?;
            let x = layer.mlp.forward(&x)?;
            let x = (x + residual)?;
            layer_in = x
        }
        let x = self.norm.forward(&layer_in)?;
        let x = x.i((.., seq_len - 1, ..))?.contiguous()?; // PATCH: contiguous so bsz>1 batched prefill works (quantized matmul rejects non-contiguous)
        let _enter = self.span_output.enter();
        self.output.forward(&x)
    }

    pub fn forward_all_logits(&mut self, x: &Tensor, index_pos: usize) -> Result<Tensor> {
        let (_b_sz, seq_len) = x.dims2()?;
        if seq_len == 0 {
            candle_core::bail!("forward_all_logits requires at least one token");
        }
        let end_pos = index_pos.checked_add(seq_len).ok_or_else(|| {
            candle_core::Error::Msg("forward_all_logits context position overflow".to_string())
        })?;
        if end_pos > MAX_SEQ_LEN {
            candle_core::bail!(
                "context length exceeded: index_pos={index_pos} + seq_len={seq_len} = \
                 {end_pos} tokens, which is beyond this model's MAX_SEQ_LEN={MAX_SEQ_LEN} \
                 ceiling  -  shorten the prompt/speculative window"
            );
        }
        let checkpoint_len = if index_pos > 0 {
            let current = self.kv_cache_len()?;
            if current != index_pos {
                candle_core::bail!(
                    "forward_all_logits KV position mismatch: cache has {current} tokens, \
                     requested index_pos={index_pos}"
                );
            }
            current
        } else {
            0
        };
        let result = (|| {
            let mask = if seq_len == 1 {
                None
            } else {
                Some(self.mask(seq_len, index_pos, x.device())?)
            };
            let seq_cap = self.next_seq_cap.take().unwrap_or(MAX_SEQ_LEN);
            let _enter = self.span.enter();
            let mut layer_in = self.tok_embeddings.forward(x)?;
            for layer in self.layers.iter_mut() {
                let x = layer_in;
                let residual = &x;
                let x = layer.attention_norm.forward(&x)?;
                let attn = layer.forward_attn(&x, mask.as_ref(), index_pos, seq_cap)?;
                let x = (attn + residual)?;

                let _enter = layer.span_mlp.enter();
                let residual = &x;
                let x = layer.ffn_norm.forward(&x)?;
                let x = layer.mlp.forward(&x)?;
                layer_in = (x + residual)?;
            }
            let x = self.norm.forward(&layer_in)?.contiguous()?;
            let _enter = self.span_output.enter();
            self.output.forward(&x)
        })();
        if result.is_err() {
            self.truncate_kv_cache(checkpoint_len)?;
        }
        result
    }

    pub fn forward_all_argmax(&mut self, x: &Tensor, index_pos: usize) -> Result<Tensor> {
        let checkpoint_len = if index_pos == 0 {
            0
        } else {
            self.kv_cache_len()?
        };
        let logits = self.forward_all_logits(x, index_pos)?;
        match logits.argmax(2) {
            Ok(tokens) => Ok(tokens),
            Err(err) => {
                self.truncate_kv_cache(checkpoint_len)?;
                Err(err)
            }
        }
    }

    pub fn forward_padded(
        &mut self,
        x: &Tensor,
        positions: &Tensor,
        mask: &Tensor,
        fresh: bool,
        seq_cap: usize,
    ) -> Result<Tensor> {
        let (_b_sz, _seq_len) = x.dims2()?;
        let max_pos = positions.flatten_all()?.max(0)?.to_scalar::<u32>()? as usize;
        if max_pos >= MAX_SEQ_LEN {
            candle_core::bail!(
                "context length exceeded: max per-row position {max_pos} reaches this model's \
                 MAX_SEQ_LEN={MAX_SEQ_LEN} ceiling  -  shorten the prompt/max_tokens or use a \
                 job type/model with a larger context window"
            );
        }
        let (cos3, sin3) = self.build_per_row_cos_sin(positions)?;
        let seq_cap = if fresh {
            self.next_seq_cap.take().unwrap_or(seq_cap.min(MAX_SEQ_LEN))
        } else {
            self.next_seq_cap.take();
            MAX_SEQ_LEN
        };
        let _enter = self.span.enter();
        let mut layer_in = self.tok_embeddings.forward(x)?;
        for layer in self.layers.iter_mut() {
            let x = layer_in;
            let residual = &x;
            let x = layer.attention_norm.forward(&x)?;
            let attn = layer.forward_attn_per_row(&x, mask, &cos3, &sin3, seq_cap, fresh)?;
            let x = (attn + residual)?;

            let _enter = layer.span_mlp.enter();
            let residual = &x;
            let x = layer.ffn_norm.forward(&x)?;
            let x = layer.mlp.forward(&x)?;
            let x = (x + residual)?;
            layer_in = x
        }
        let x = self.norm.forward(&layer_in)?.contiguous()?;
        let _enter = self.span_output.enter();
        self.output.forward(&x)
    }

    fn build_per_row_cos_sin(&self, positions: &Tensor) -> Result<(Tensor, Tensor)> {
        let (b_sz, seq_len) = positions.dims2()?;
        let layer0 = self
            .layers
            .first()
            .ok_or_else(|| candle_core::Error::Msg("no layers".into()))?;
        let half = layer0.cos.dim(1)?;
        let flat = positions.flatten_all()?.contiguous()?;
        let cos = layer0.cos.index_select(&flat, 0)?; // (b*seq, half)
        let sin = layer0.sin.index_select(&flat, 0)?;
        let cos3 = cos.reshape((b_sz, seq_len, half))?.contiguous()?;
        let sin3 = sin.reshape((b_sz, seq_len, half))?.contiguous()?;
        Ok((cos3, sin3))
    }
}

#[cfg(test)]
mod tests {
    use super::{KvCacheSlot, MAX_SEQ_LEN};
    use candle_core::{Device, IndexOp, Result, Tensor};
    use candle_transformers::utils::build_causal_mask;

    fn ramp(b: usize, h: usize, s: usize, d: usize, base: f32) -> Result<Tensor> {
        let n = b * h * s * d;
        let data: Vec<f32> = (0..n).map(|i| base + i as f32).collect();
        Tensor::from_vec(data, (b, h, s, d), &Device::Cpu)
    }

    #[test]
    fn prealloc_kv_append_matches_cat() -> Result<()> {
        let (b, h, d) = (3usize, 2usize, 4usize);

        let prefill = ramp(b, h, 5, d, 1.0)?;
        let mut cat_cache = prefill.clone();

        let mut slot = KvCacheSlot::new();
        let appended = slot.append(&prefill)?;
        assert_eq!(
            appended.flatten_all()?.to_vec1::<f32>()?,
            cat_cache.flatten_all()?.to_vec1::<f32>()?,
            "prefill append must equal the input"
        );

        for step in 0..6 {
            let tok = ramp(b, h, 1, d, 1000.0 + step as f32 * 100.0)?;
            cat_cache = Tensor::cat(&[&cat_cache, &tok], 2)?;
            let got = slot.append(&tok)?;
            assert_eq!(
                got.dims(),
                cat_cache.dims(),
                "shape must match cat at step {step}"
            );
            assert_eq!(
                got.flatten_all()?.to_vec1::<f32>()?,
                cat_cache.flatten_all()?.to_vec1::<f32>()?,
                "slice_set append must be byte-identical to cat at step {step}"
            );
        }
        Ok(())
    }

    #[test]
    fn prealloc_kv_reset_starts_fresh() -> Result<()> {
        let (h, d) = (2usize, 4usize);
        let mut slot = KvCacheSlot::new();

        let first = ramp(4, h, 3, d, 1.0)?;
        let out1 = slot.append(&first)?;
        assert_eq!(out1.dims(), [4, h, 3, d]);

        slot.reset();
        let second = ramp(2, h, 2, d, 500.0)?;
        let out2 = slot.append(&second)?;
        assert_eq!(
            out2.dims(),
            [2, h, 2, d],
            "reset must reallocate at the new batch size"
        );
        assert_eq!(
            out2.flatten_all()?.to_vec1::<f32>()?,
            second.flatten_all()?.to_vec1::<f32>()?,
            "after reset the slot holds only the new prefill"
        );
        Ok(())
    }

    #[test]
    fn prealloc_kv_truncate_matches_never_speculated_path() -> Result<()> {
        let (b, h, d) = (2usize, 2usize, 4usize);
        let prefix = ramp(b, h, 3, d, 1.0)?;
        let proposals = ramp(b, h, 4, d, 1000.0)?;
        let correction = ramp(b, h, 1, d, 9000.0)?;

        let mut speculative = KvCacheSlot::new();
        speculative.append(&prefix)?;
        speculative.append(&proposals)?;
        speculative.truncate(5)?; // accept two of four proposal positions
        let got = speculative.append(&correction)?;

        let mut reference = KvCacheSlot::new();
        reference.append(&prefix)?;
        reference.append(&proposals.narrow(2, 0, 2)?)?;
        let expected = reference.append(&correction)?;

        assert_eq!(
            got.flatten_all()?.to_vec1::<f32>()?,
            expected.flatten_all()?.to_vec1::<f32>()?,
            "truncate + overwrite must equal a cache that never exposed rejected KV"
        );
        let err = speculative.truncate(7).unwrap_err();
        assert!(
            err.to_string().contains("cannot grow"),
            "unexpected error: {err}"
        );
        Ok(())
    }

    #[test]
    fn prealloc_kv_right_sized_cap_matches_cat_and_shrinks_buffer() -> Result<()> {
        let (b, h, d) = (2usize, 2usize, 4usize);
        let small_cap = 8usize; // stand-in for a real (prompt_len + max_tokens) estimate

        let mut slot = KvCacheSlot::new();
        slot.reset_with_cap(small_cap);

        let prefill = ramp(b, h, 3, d, 1.0)?;
        let mut cat_cache = prefill.clone();
        let appended = slot.append(&prefill)?;
        assert_eq!(
            appended.flatten_all()?.to_vec1::<f32>()?,
            cat_cache.flatten_all()?.to_vec1::<f32>()?,
            "right-sized prefill append must equal the input"
        );
        assert_eq!(
            slot.buf.as_ref().unwrap().dim(2)?,
            small_cap,
            "a right-sized reset must allocate at the given cap, not MAX_SEQ_LEN"
        );

        for step in 0..10 {
            let tok = ramp(b, h, 1, d, 1000.0 + step as f32 * 100.0)?;
            cat_cache = Tensor::cat(&[&cat_cache, &tok], 2)?;
            let got = slot.append(&tok)?;
            assert_eq!(
                got.dims(),
                cat_cache.dims(),
                "shape must match cat at step {step} even past the original cap"
            );
            assert_eq!(
                got.flatten_all()?.to_vec1::<f32>()?,
                cat_cache.flatten_all()?.to_vec1::<f32>()?,
                "right-sized append must stay byte-identical to cat at step {step}, including past cap"
            );
        }
        assert!(slot.buf.as_ref().unwrap().dim(2)? <= MAX_SEQ_LEN);
        Ok(())
    }

    #[test]
    fn prealloc_kv_compact_keeps_rows_verbatim() -> Result<()> {
        let (h, d) = (2usize, 3usize);
        let mut slot = KvCacheSlot::new();

        let prefill = ramp(4, h, 2, d, 1.0)?;
        slot.append(&prefill)?;
        let step = ramp(4, h, 1, d, 9000.0)?;
        let full = slot.append(&step)?; // (4, h, 3, d)

        let keep = [0u32, 2u32];
        let idx = Tensor::from_vec(keep.to_vec(), keep.len(), &Device::Cpu)?;
        slot.compact(&idx)?;

        let expected = full.index_select(&idx, 0)?;
        let next = ramp(2, h, 1, d, 7777.0)?;
        let after = slot.append(&next)?; // (2, h, 4, d)
        let live_prefix = after.narrow(2, 0, 3)?;
        assert_eq!(
            live_prefix.flatten_all()?.to_vec1::<f32>()?,
            expected.flatten_all()?.to_vec1::<f32>()?,
            "compact must keep the named rows' KV bytewise identical"
        );
        Ok(())
    }

    #[test]
    fn snapshot_restore_continues_prefix_verbatim() -> Result<()> {
        let (h, d) = (2usize, 4usize);

        let prefix = ramp(1, h, 5, d, 1.0)?;
        let remainder = ramp(1, h, 3, d, 500.0)?;
        let mut inline = KvCacheSlot::new();
        inline.append(&prefix)?;
        let inline_full = inline.append(&remainder)?; // (1, h, 8, d)

        let mut shared = KvCacheSlot::new();
        shared.append(&prefix)?;
        let snap = shared.snapshot()?.expect("prefix snapshot");
        assert_eq!(snap.1, 5, "snapshot length is the prefix length");

        for item in 0..2 {
            let mut forked = KvCacheSlot::new();
            forked.append(&ramp(1, h, 2, d, 9000.0)?)?;
            forked.restore(&snap)?;
            let forked_full = forked.append(&remainder)?; // (1, h, 8, d)
            assert_eq!(
                forked_full.dims(),
                inline_full.dims(),
                "item {item}: forked shape must match inline"
            );
            assert_eq!(
                forked_full.flatten_all()?.to_vec1::<f32>()?,
                inline_full.flatten_all()?.to_vec1::<f32>()?,
                "item {item}: forked prefix+remainder must be byte-identical to inline"
            );
        }
        assert_eq!(snap.1, 5, "snapshot length unchanged after forks");
        Ok(())
    }

    #[test]
    fn restore_broadcast_matches_per_item_restore() -> Result<()> {
        let (h, d) = (2usize, 4usize);
        let b_sz = 4usize;

        let prefix = ramp(1, h, 5, d, 1.0)?;
        let mut shared = KvCacheSlot::new();
        shared.append(&prefix)?;
        let snap = shared.snapshot()?.expect("prefix snapshot");

        let remainder_row = ramp(1, h, 3, d, 500.0)?;
        let mut refs: Vec<Tensor> = Vec::with_capacity(b_sz);
        for _ in 0..b_sz {
            let mut forked = KvCacheSlot::new();
            forked.restore(&snap)?;
            refs.push(forked.append(&remainder_row)?); // (1, h, 8, d)
        }

        let remainder_batch = remainder_row.repeat((b_sz, 1, 1, 1))?;
        let mut batched = KvCacheSlot::new();
        batched.restore_broadcast(&snap, b_sz)?;
        let batched_full = batched.append(&remainder_batch)?; // (b_sz, h, 8, d)

        assert_eq!(batched_full.dims(), [b_sz, h, 8, d]);
        for row in 0..b_sz {
            let got = batched_full.i(row)?.flatten_all()?.to_vec1::<f32>()?;
            let want = refs[row].i(0)?.flatten_all()?.to_vec1::<f32>()?;
            assert_eq!(
                got, want,
                "row {row}: broadcast-fork must be byte-identical to a lone fork"
            );
        }
        assert_eq!(snap.1, 5, "snapshot length unchanged after broadcast fork");
        Ok(())
    }

    #[test]
    fn restore_broadcast_rejects_non_unit_batch_snapshot() -> Result<()> {
        let (h, d) = (2usize, 4usize);
        let prefix = ramp(2, h, 5, d, 1.0)?; // batch dim 2, not 1
        let mut slot = KvCacheSlot::new();
        slot.append(&prefix)?;
        let snap = slot.snapshot()?.expect("prefix snapshot");
        let mut target = KvCacheSlot::new();
        let err = target.restore_broadcast(&snap, 4).unwrap_err();
        assert!(
            err.to_string().contains("batch dim must be 1"),
            "unexpected error: {err}"
        );
        Ok(())
    }

    #[test]
    fn causal_mask_square_shape() -> Result<()> {
        let mask = build_causal_mask(4, 0, &Device::Cpu)?;
        assert_eq!(mask.dims(), [4, 4]);
        Ok(())
    }

    #[test]
    fn causal_mask_rectangular_shape() -> Result<()> {
        let mask = build_causal_mask(4, 65, &Device::Cpu)?;
        assert_eq!(mask.dims(), [4, 69]);
        Ok(())
    }

    #[test]
    fn causal_mask_square_values() -> Result<()> {
        let mask = build_causal_mask(3, 0, &Device::Cpu)?;
        let data: Vec<u8> = mask.flatten_all()?.to_vec1()?;
        assert_eq!(data, [0, 1, 1, 0, 0, 1, 0, 0, 0]);
        Ok(())
    }

    #[test]
    fn causal_mask_rectangular_values() -> Result<()> {
        let mask = build_causal_mask(3, 2, &Device::Cpu)?;
        let data: Vec<u8> = mask.flatten_all()?.to_vec1()?;
        #[rustfmt::skip]
        assert_eq!(data, [
            0, 0,  0, 1, 1,
            0, 0,  0, 0, 1,
            0, 0,  0, 0, 0,
        ]);
        Ok(())
    }

    #[test]
    fn causal_mask_single_query_with_prefix() -> Result<()> {
        let mask = build_causal_mask(1, 10, &Device::Cpu)?;
        assert_eq!(mask.dims(), [1, 11]);
        let data: Vec<u8> = mask.flatten_all()?.to_vec1()?;
        assert!(
            data.iter().all(|&v| v == 0),
            "single-query mask should be all-zero"
        );
        Ok(())
    }

    #[test]
    fn causal_mask_broadcasts_to_attention_shape() -> Result<()> {
        let batch = 1usize;
        let heads = 8usize;
        let seq_len = 4usize;
        let index_pos = 10usize;

        let mask = build_causal_mask(seq_len, index_pos, &Device::Cpu)?;
        let kv_len = index_pos + seq_len;
        let att_shape = &[batch, heads, seq_len, kv_len];
        let broadcasted = mask.broadcast_as(att_shape.as_slice())?;
        assert_eq!(broadcasted.dims(), att_shape);
        Ok(())
    }

    #[test]
    fn mask_cache_is_bounded_and_recompute_is_identical() -> Result<()> {
        use std::collections::{HashMap, VecDeque};

        let device = Device::Cpu;
        let mut masks: HashMap<(usize, usize), Tensor> = HashMap::new();
        let mut order: VecDeque<(usize, usize)> = VecDeque::new();

        let first_index_pos = 0usize;
        let first_seq_len = 1usize;
        let first_bytes: Vec<u8> = build_causal_mask(first_seq_len, first_index_pos, &device)?
            .flatten_all()?
            .to_vec1()?;

        let inserts = super::MASK_CACHE_CAP * 3;
        for i in 0..inserts {
            let seq_len = 1usize;
            let index_pos = i; // distinct kv_len each iteration
            let kv_len = index_pos + seq_len;
            let key = (seq_len, kv_len);
            if !masks.contains_key(&key) {
                let mask = build_causal_mask(seq_len, index_pos, &device)?;
                while order.len() >= super::MASK_CACHE_CAP {
                    if let Some(old) = order.pop_front() {
                        masks.remove(&old);
                    } else {
                        break;
                    }
                }
                masks.insert(key, mask);
                order.push_back(key);
            }
            assert!(masks.len() <= super::MASK_CACHE_CAP, "cache exceeded cap");
            assert_eq!(masks.len(), order.len(), "map and order out of sync");
        }

        assert!(
            !masks.contains_key(&(first_seq_len, first_index_pos + first_seq_len)),
            "oldest key should have been evicted"
        );

        let recomputed: Vec<u8> = build_causal_mask(first_seq_len, first_index_pos, &device)?
            .flatten_all()?
            .to_vec1()?;
        assert_eq!(recomputed, first_bytes, "recomputed mask must be identical");
        Ok(())
    }

    #[test]
    fn padded_mask_reduces_to_causal_mask_when_unpadded() -> Result<()> {
        let device = Device::Cpu;
        for seq_len in [1usize, 3, 5, 8] {
            for b_sz in [1usize, 2, 4] {
                let real_len = vec![seq_len; b_sz];
                let q_global_pos: Vec<Vec<usize>> =
                    (0..b_sz).map(|_| (0..seq_len).collect()).collect();
                let padded =
                    super::build_padded_mask(&real_len, &q_global_pos, 0, seq_len, &device)?;
                assert_eq!(padded.dims(), [b_sz, 1, seq_len, seq_len]);
                let causal: Vec<u8> = build_causal_mask(seq_len, 0, &device)?
                    .flatten_all()?
                    .to_vec1()?;
                for r in 0..b_sz {
                    let row: Vec<u8> = padded.i((r, 0))?.contiguous()?.flatten_all()?.to_vec1()?;
                    assert_eq!(
                        row, causal,
                        "row {r} of the padded mask (seq_len={seq_len}, b_sz={b_sz}) must equal build_causal_mask"
                    );
                }
            }
        }
        Ok(())
    }

    #[test]
    fn padded_mask_forbids_pad_columns_per_row() -> Result<()> {
        let device = Device::Cpu;
        let seq_len = 5usize;
        let real_len = vec![5usize, 3usize];
        let q_global_pos: Vec<Vec<usize>> = vec![(0..5).collect(), (0..5).collect()];
        let mask = super::build_padded_mask(&real_len, &q_global_pos, 0, seq_len, &device)?;
        let m: Vec<Vec<u8>> = (0..2)
            .map(|r| mask.i((r, 0))?.contiguous()?.flatten_all()?.to_vec1::<u8>())
            .collect::<Result<_>>()?;
        let want0: Vec<u8> = (0..5)
            .flat_map(|i| (0..5).map(move |j| u8::from(j > i)))
            .collect();
        assert_eq!(m[0], want0, "row 0 (unpadded) must be plain causal");
        for i in 0..5usize {
            for j in 0..5usize {
                let got = m[1][i * 5 + j];
                let want = u8::from(j > i); // right-padding ⇒ causality alone excludes pads
                assert_eq!(
                    got, want,
                    "row1 q{i} k{j}: right-padded causal mask mismatch (pad cols must be forbidden for real queries)"
                );
            }
        }
        Ok(())
    }

    #[test]
    fn rope_per_row_equals_scalar_when_positions_uniform() -> Result<()> {
        let device = Device::Cpu;
        let (b_sz, n_head, seq_len, head_dim) = (2usize, 4usize, 6usize, 8usize);
        let half = head_dim / 2;
        let table_len = 64usize;
        let cos_tab = Tensor::from_vec(
            (0..table_len * half)
                .map(|i| (i as f32 * 0.013).cos())
                .collect::<Vec<f32>>(),
            (table_len, half),
            &device,
        )?;
        let sin_tab = Tensor::from_vec(
            (0..table_len * half)
                .map(|i| (i as f32 * 0.017).sin())
                .collect::<Vec<f32>>(),
            (table_len, half),
            &device,
        )?;
        let x = Tensor::from_vec(
            (0..b_sz * n_head * seq_len * head_dim)
                .map(|i| (i as f32 * 0.001) - 0.5)
                .collect::<Vec<f32>>(),
            (b_sz, n_head, seq_len, head_dim),
            &device,
        )?;

        for index_pos in [0usize, 7, 40] {
            let cos2 = cos_tab.narrow(0, index_pos, seq_len)?;
            let sin2 = sin_tab.narrow(0, index_pos, seq_len)?;
            let scalar = candle_nn::rotary_emb::rope_i(&x.contiguous()?, &cos2, &sin2)?;

            let positions: Vec<u32> = (0..b_sz)
                .flat_map(|_| (index_pos..index_pos + seq_len).map(|p| p as u32))
                .collect();
            let pos = Tensor::from_vec(positions, (b_sz, seq_len), &device)?;
            let flat = pos.flatten_all()?.contiguous()?;
            let cos3 = cos_tab
                .index_select(&flat, 0)?
                .reshape((b_sz, seq_len, half))?
                .contiguous()?;
            let sin3 = sin_tab
                .index_select(&flat, 0)?
                .reshape((b_sz, seq_len, half))?
                .contiguous()?;
            let per_row = candle_nn::rotary_emb::rope_i(&x.contiguous()?, &cos3, &sin3)?;

            let a: Vec<f32> = scalar.flatten_all()?.to_vec1()?;
            let b: Vec<f32> = per_row.flatten_all()?.to_vec1()?;
            assert_eq!(
                a, b,
                "per-row rope (index_select 3D cos/sin) must byte-equal scalar rope (narrow 2D cos/sin) at index_pos={index_pos}"
            );
        }
        Ok(())
    }
}
