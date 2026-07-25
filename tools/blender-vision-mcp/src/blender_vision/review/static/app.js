const state = { snapshot: null };
const token = document.querySelector('meta[name="bvmcp-token"]').content;
const $ = (selector) => document.querySelector(selector);
const $$ = (selector) => [...document.querySelectorAll(selector)];

function text(value) {
  return value === null || value === undefined ? "—" : String(value);
}

function pretty(value) {
  return JSON.stringify(value ?? null, null, 2);
}

function toast(message) {
  const target = $("#toast");
  target.textContent = message;
  target.classList.add("visible");
  setTimeout(() => target.classList.remove("visible"), 3200);
}

async function action(name, payload = {}) {
  const response = await fetch(`/api/action/${name}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-BVMCP-Review-Token": token },
    body: JSON.stringify(payload),
  });
  const value = await response.json();
  if (!response.ok) throw new Error(value.error || `HTTP ${response.status}`);
  toast(`${name} recorded`);
  await load();
  return value.result;
}

function renderHeader(data) {
  $("#project-title").textContent = data.project.name;
  $("#fidelity").textContent = `Target ${data.project.target_fidelity}`;
  const receipt = data.latest_receipt;
  const target = $("#receipt-state");
  target.textContent = receipt ? (receipt.accepted ? "Accepted" : "Blocked") : "No receipt";
  target.className = `pill ${receipt?.accepted ? "" : receipt ? "bad" : "neutral"}`;
}

function renderComparison(data) {
  const select = $("#comparison-select");
  select.replaceChildren();
  data.comparisons.forEach((item, index) => {
    const option = document.createElement("option");
    option.value = String(index);
    option.textContent = `${item.reference_id.slice(0, 8)} · IoU ${text(item.metrics.silhouette_iou)}`;
    select.append(option);
  });
  const show = (index = 0) => {
    const item = data.comparisons[index];
    $("#comparison-empty").classList.toggle("hidden", Boolean(item));
    $("#comparison-workbench").classList.toggle("hidden", !item);
    if (!item) return;
    $("#reference-image").src = item.reference_url || "";
    $("#render-image").src = item.render_url || "";
    $("#render-overlay").src = item.render_url || "";
    $("#residual-image").src = item.residual_url || "";
    const metrics = $("#comparison-metrics");
    metrics.replaceChildren();
    Object.entries(item.metrics).forEach(([name, value]) => {
      if (typeof value === "object") return;
      const cell = document.createElement("div");
      const dt = document.createElement("dt");
      const dd = document.createElement("dd");
      dt.textContent = name.replaceAll("_", " ");
      dd.textContent = text(value);
      cell.append(dt, dd);
      metrics.append(cell);
    });
  };
  select.onchange = () => show(Number(select.value));
  show(0);
}

function updateWipe() {
  const wipe = Number($("#wipe-position").value);
  $("#render-clip").style.right = `${100 - wipe}%`;
  $("#wipe-line").style.left = `${wipe}%`;
  $("#render-overlay").style.opacity = Number($("#overlay-opacity").value) / 100;
}

function digestCards(digests, label) {
  return (digests || []).map((digest) => {
    const card = document.createElement("div");
    card.className = "artifact-card";
    const heading = document.createElement("strong");
    heading.textContent = label;
    const link = document.createElement("a");
    link.href = `/artifact/${digest}`;
    link.textContent = digest;
    link.target = "_blank";
    card.append(heading, document.createElement("br"), link);
    if (["Depth", "Normal", "Mask", "Confidence", "Visibility"].includes(label)) {
      const image = document.createElement("img");
      image.src = `/artifact/${digest}`;
      image.alt = `${label} artifact`;
      image.onerror = () => image.remove();
      card.append(image);
    }
    return card;
  });
}

function renderGeometry(data) {
  const summary = $("#geometry-summary");
  summary.replaceChildren();
  const modalities = ["depth", "point", "normal", "correspondence", "visibility", "confidence", "mask"];
  modalities.forEach((name) => {
    const count = data.geometry_runs.reduce((total, run) => total + (run.evidence[`${name}_artifacts`]?.length || 0), 0);
    const card = document.createElement("div");
    card.className = "summary-card";
    card.innerHTML = `<strong>${count}</strong><span>${name} artifacts</span>`;
    summary.append(card);
  });
  const artifacts = $("#geometry-artifacts");
  artifacts.replaceChildren();
  const fields = { depth: "Depth", point: "Point cloud", normal: "Normal", correspondence: "Correspondence", visibility: "Visibility", confidence: "Confidence", mask: "Mask" };
  data.geometry_runs.forEach((run) => Object.entries(fields).forEach(([field, label]) => {
    digestCards(run.evidence[`${field}_artifacts`], `${run.backend} · ${label}`).forEach((card) => artifacts.append(card));
  }));
  if (!artifacts.children.length) artifacts.textContent = "No depth, normal, point, or visibility artifacts yet.";
  $("#geometry-consensus").textContent = pretty(data.geometry_consensus);
}

function renderTrees(data) {
  const featureRoot = $("#feature-graph");
  featureRoot.replaceChildren();
  const grouped = data.features.reduce((result, item) => {
    const key = item.parent_component || "unassigned";
    (result[key] ||= []).push(item);
    return result;
  }, {});
  Object.entries(grouped).forEach(([parent, features]) => {
    const node = document.createElement("div");
    node.className = "tree-node";
    node.innerHTML = `<strong>${parent}</strong><small>${features.length} feature(s)</small>`;
    features.forEach((feature) => {
      const child = document.createElement("div");
      child.className = "tree-node";
      child.innerHTML = `${feature.type} · ${(feature.confidence * 100).toFixed(0)}%<small>${feature.evidence_class} · ${feature.approval?.state || "pending"}</small>`;
      node.append(child);
    });
    featureRoot.append(node);
  });
  if (!data.features.length) featureRoot.textContent = "No technical features recorded.";
  const components = $("#component-tree");
  components.replaceChildren();
  data.components.forEach((component) => {
    const node = document.createElement("div");
    node.className = "tree-node";
    node.innerHTML = `<strong>${component.id}</strong><small>${component.type} · revision ${component.revision}</small><code>${JSON.stringify(component.parameters)}</code>`;
    components.append(node);
  });
  if (!data.components.length) components.textContent = "No parametric components recorded.";
}

function fillSelect(selector, items, label) {
  const select = $(selector);
  select.replaceChildren();
  items.forEach((item) => {
    const option = document.createElement("option");
    option.value = item.id;
    option.textContent = label(item);
    select.append(option);
  });
}

function renderMeasurements(data) {
  const root = $("#measurement-list");
  root.replaceChildren();
  data.measurements.forEach((measurement) => {
    const card = document.createElement("div"); card.className = "tree-node";
    const title = document.createElement("strong"); title.textContent = measurement.type;
    const detail = document.createElement("small");
    detail.textContent = `${measurement.evidence_class} · ${measurement.certainty} · ${measurement.coordinate_frame}`;
    const value = document.createElement("code"); value.textContent = JSON.stringify(measurement.value);
    card.append(title, detail, value); root.append(card);
  });
  if (!data.measurements.length) root.textContent = "No measurements recorded.";
  const grids = $("#grid-list"); grids.replaceChildren();
  data.measurement_grids.forEach((grid) => {
    const card = document.createElement("div"); card.className = "tree-node";
    const title = document.createElement("strong"); title.textContent = `Perspective grid ${grid.id.slice(0, 8)}`;
    const detail = document.createElement("small");
    detail.textContent = `${grid.reference_id.slice(0, 8)} · ${Object.keys(grid.definition.vanishing_points).length} vanishing axes · ${grid.created_by}`;
    card.append(title, detail); grids.append(card);
  });
  if (!data.measurement_grids.length) grids.textContent = "No perspective grids recorded.";
  fillSelect("#measurement-correct-id", data.measurements, (item) => `${item.type} · ${item.id.slice(0, 8)}`);
  fillSelect("#feature-link-id", data.features, (item) => `${item.type} · ${item.id.slice(0, 8)}`);
  fillSelect("#feature-link-reference", data.references.filter((item) => item.media_type.startsWith("image/")), (item) => item.original_name);
  $("#measurement-correct-form").onsubmit = async (event) => {
    event.preventDefault();
    try {
      await action("measurement.correct", {
        id: $("#measurement-correct-id").value,
        value: JSON.parse($("#measurement-correct-value").value),
        uncertainty: JSON.parse($("#measurement-correct-uncertainty").value),
        reviewer: $("#measurement-correct-reviewer").value,
        reason: $("#measurement-correct-reason").value,
      });
    } catch (error) { toast(error.message); }
  };
  $("#feature-link-form").onsubmit = async (event) => {
    event.preventDefault();
    try {
      await action("feature.link", {
        id: $("#feature-link-id").value,
        reference_id: $("#feature-link-reference").value,
        observation: JSON.parse($("#feature-link-observation").value),
        reviewer: $("#feature-link-reviewer").value,
        reason: $("#feature-link-reason").value,
      });
    } catch (error) { toast(error.message); }
  };
  $("#capture-request-form").onsubmit = async (event) => {
    event.preventDefault();
    try {
      await action("capture.request", {
        direction: $("#capture-direction").value,
        region: $("#capture-region").value,
        instructions: $("#capture-instructions").value,
        reviewer: $("#capture-reviewer").value,
        reason: $("#capture-reason").value,
      });
    } catch (error) { toast(error.message); }
  };
  $("#tier-review-form").onsubmit = async (event) => {
    event.preventDefault();
    try {
      await action("tier.review", {
        fidelity: $("#tier-fidelity").value,
        accepted: $("#tier-decision").value === "true",
        reviewer: $("#tier-reviewer").value,
        reason: $("#tier-reason").value,
      });
    } catch (error) { toast(error.message); }
  };
}

function renderCameras(data) {
  const list = $("#camera-list");
  list.replaceChildren();
  data.cameras.forEach((solution) => {
    const classes = [...new Set(solution.solution.cameras.map((camera) => camera.registration_class))];
    const card = document.createElement("div");
    card.className = "summary-card";
    card.innerHTML = `<strong>${solution.backend}</strong><span>${solution.solution.cameras.length} view(s) · ${classes.join(", ")} · ${solution.approved ? "approved" : "pending"}</span>`;
    list.append(card);
  });
  const svg = $("#camera-map");
  svg.replaceChildren();
  const cameras = data.cameras.flatMap((solution) => solution.solution.cameras);
  cameras.forEach((camera, index) => {
    const center = camera.world_from_camera.map((row) => row[3]).slice(0, 3);
    const x = 400 + Math.max(-330, Math.min(330, center[0] * .45));
    const y = 180 + Math.max(-140, Math.min(140, center[1] * .28));
    const group = document.createElementNS("http://www.w3.org/2000/svg", "g");
    group.innerHTML = `<line x1="400" y1="180" x2="${x}" y2="${y}" stroke="#506078"/><path d="M ${x - 8} ${y - 6} L ${x + 8} ${y} L ${x - 8} ${y + 6} Z" fill="#78a8ff"/><text x="${x + 10}" y="${y - 7}" fill="#dce5f4" font-size="11">${index + 1}</text>`;
    svg.append(group);
  });
  const object = document.createElementNS("http://www.w3.org/2000/svg", "rect");
  object.setAttribute("x", "365"); object.setAttribute("y", "145"); object.setAttribute("width", "70"); object.setAttribute("height", "70"); object.setAttribute("rx", "12"); object.setAttribute("fill", "#303744");
  svg.append(object);
  $("#camera-consensus").textContent = pretty(data.camera_consensus);
}

function renderCoverage(data) {
  const root = $("#coverage-map");
  root.replaceChildren();
  const report = data.coverage || {};
  const covered = new Set(report.covered_directions || report.observed_directions || []);
  ["front", "rear", "left", "right", "top", "bottom"].forEach((direction) => {
    const cell = document.createElement("div");
    cell.className = `coverage-cell ${covered.has(direction) ? "observed" : "missing"}`;
    cell.innerHTML = `<strong>${direction}</strong><br><small>${covered.has(direction) ? "observed" : "capture requested"}</small>`;
    root.append(cell);
  });
  const uncertainty = $("#uncertainty-map");
  uncertainty.replaceChildren();
  const items = data.features.filter((feature) => feature.confidence < .8 || ["OCCLUDED", "UNSEEN", "INFERRED_LOW_CONFIDENCE"].includes(feature.evidence_class));
  items.forEach((feature) => {
    const node = document.createElement("div");
    node.className = "tree-node";
    node.innerHTML = `<strong>${feature.type}</strong><small>${feature.evidence_class} · confidence ${(feature.confidence * 100).toFixed(0)}% · ${feature.coverage_group || "ungrouped"}</small>`;
    uncertainty.append(node);
  });
  if (!items.length) uncertainty.textContent = "No high-priority feature uncertainty is recorded.";
}

function renderAcceptance(data) {
  const receipt = data.latest_receipt;
  const banner = $("#acceptance-banner");
  const checklist = $("#acceptance-checklist");
  checklist.replaceChildren();
  if (!receipt) {
    banner.className = "acceptance-banner neutral";
    banner.textContent = "No acceptance receipt has been exported.";
    $("#receipt-verification").textContent = "";
    return;
  }
  banner.className = `acceptance-banner ${receipt.acceptance.accepted ? "good" : "bad"}`;
  banner.textContent = receipt.acceptance.accepted ? `Accepted at ${receipt.acceptance.accepted_fidelity}` : `${receipt.acceptance.blockers.length} blocker(s) prevent ${receipt.acceptance.target_fidelity}`;
  const blockers = receipt.acceptance.blockers;
  if (!blockers.length) {
    const item = document.createElement("div"); item.className = "check-item"; item.textContent = "Every recorded gate passed."; checklist.append(item);
  } else blockers.forEach((blocker) => {
    const item = document.createElement("div"); item.className = "check-item blocked"; item.textContent = blocker; checklist.append(item);
  });
  $("#receipt-verification").textContent = pretty(receipt.verification);
}

function actionName(item, accepted) {
  if (item.kind === "repair" && item.state === "proposed") {
    return accepted ? "repair.approve" : "repair.reject_proposal";
  }
  return { feature: "feature.review", camera: "camera.review", repair: "repair.review", fit: "fit.review", material: "material.review", optimization: "optimization.review", reference_mask: "reference_mask.review" }[item.kind];
}

function appendReferenceAdoptionReview(card, item) {
  const warning = document.createElement("div"); warning.className = "reference-adoption-warning";
  warning.textContent = "This legacy image has verified stored bytes but no governed source-ledger record. Its old rights label is context only.";
  card.append(warning);
  if (item.image_url) {
    const image = document.createElement("img"); image.className = "reference-adoption-image"; image.src = item.image_url; image.alt = item.reference.original_name;
    card.append(image);
  }
  const facts = document.createElement("div"); facts.className = "reference-adoption-facts";
  facts.textContent = `${item.reference.viewpoint_label || "unlabelled view"} · legacy label ${item.reference.rights_state || "unknown"} · ${item.reference.artifact_digest}`;
  card.append(facts);
  const limitations = document.createElement("ul"); limitations.className = "landmark-limitations";
  item.known_limitations.forEach((value) => { const row = document.createElement("li"); row.textContent = value; limitations.append(row); });
  card.append(limitations);

  const form = document.createElement("form"); form.className = "reference-adoption-form";
  const sourceGrid = document.createElement("div"); sourceGrid.className = "reference-adoption-grid";
  const addField = (labelText, input) => {
    const label = document.createElement("label"); const caption = document.createElement("span"); caption.textContent = labelText;
    label.append(caption, input); sourceGrid.append(label); return input;
  };
  const input = (placeholder = "") => { const node = document.createElement("input"); node.placeholder = placeholder; return node; };
  const origin = addField("Origin or local provenance", input("Exact URL, archive, repository, or owner"));
  const publisher = addField("Publisher / owner", input("Named publisher or owner"));
  const pageTitle = addField("Page / asset title", input()); pageTitle.value = item.suggested_source.page_title || "";
  const authorityClass = addField("Authority class", input("manufacturer, independent review, user-owned…"));
  const viewpoint = addField("Viewpoint", input()); viewpoint.value = item.suggested_source.viewpoint || "";
  const qualityScore = addField("Quality score (0–1)", input("0.0–1.0")); qualityScore.type = "number"; qualityScore.min = "0"; qualityScore.max = "1"; qualityScore.step = "0.01";
  const targetVariant = addField("Target variant JSON", input("{}")); targetVariant.value = JSON.stringify(item.suggested_source.target_variant && Object.keys(item.suggested_source.target_variant).length ? item.suggested_source.target_variant : (item.canonical_target || {}));
  const sourceUrl = addField("Source URL (optional)", input("https://…")); sourceUrl.type = "url";
  const rightsStatus = addField("Rights status", input("Explicit reviewed status"));
  const rightsNotes = addField("Rights notes (optional)", input());
  const governanceValues = [["", "Choose review"], ["approved", "Approved"], ["not_applicable", "Not applicable"], ["user_owned", "User owned"]];
  const select = () => { const node = document.createElement("select"); governanceValues.forEach(([value, text]) => { const option = document.createElement("option"); option.value = value; option.textContent = text; node.append(option); }); return node; };
  const terms = addField("Source terms review", select());
  const privacy = addField("Privacy review", select());
  form.append(sourceGrid);

  const checks = document.createElement("div"); checks.className = "reference-adoption-checks";
  const internalUse = document.createElement("input"); internalUse.type = "checkbox";
  const internalLabel = document.createElement("label"); internalLabel.append(internalUse, document.createTextNode(" Explicit internal-use permission"));
  const redistribution = document.createElement("input"); redistribution.type = "checkbox";
  const redistributionLabel = document.createElement("label"); redistributionLabel.append(redistribution, document.createTextNode(" Redistribution permitted"));
  checks.append(internalLabel, redistributionLabel); form.append(checks);

  const decisionGrid = document.createElement("div"); decisionGrid.className = "reference-adoption-decision";
  const reviewer = input("Named reviewer"); reviewer.required = true;
  const reason = input("Evidence-bound reason"); reason.required = true;
  const adopt = document.createElement("button"); adopt.type = "button"; adopt.className = "approve"; adopt.textContent = "Adopt into source ledger";
  const exclude = document.createElement("button"); exclude.type = "button"; exclude.className = "reject"; exclude.textContent = "Exclude legacy image";
  decisionGrid.append(reviewer, reason, adopt, exclude); form.append(decisionGrid);
  const identityValid = () => reviewer.reportValidity() && reason.reportValidity();
  adopt.onclick = async () => {
    if (!identityValid()) return;
    const required = [origin, publisher, pageTitle, authorityClass, viewpoint, qualityScore, targetVariant, rightsStatus, terms, privacy];
    if (required.some((node) => !String(node.value).trim())) { toast("Complete every source, governance, and rights field before adoption."); return; }
    try {
      const variant = JSON.parse(targetVariant.value);
      if (!variant || Array.isArray(variant) || typeof variant !== "object") throw new Error("Target variant must be a JSON object.");
      const quality = Number(qualityScore.value);
      if (!Number.isFinite(quality) || quality < 0 || quality > 1) throw new Error("Quality score must be between zero and one.");
      await action("reference_adoption.review", {
        id: item.id, decision: "ADOPT", reviewer: reviewer.value, reason: reason.value,
        source: { origin: origin.value, publisher: publisher.value, page_title: pageTitle.value, authority_class: authorityClass.value, target_variant: variant, viewpoint: viewpoint.value, quality_score: quality, url: sourceUrl.value || null },
        rights: { status: rightsStatus.value, internal_use: internalUse.checked, redistribution: redistribution.checked, notes: rightsNotes.value || null },
        source_terms_review: terms.value, privacy_review: privacy.value,
      });
    } catch (error) { toast(error.message); }
  };
  exclude.onclick = async () => {
    if (!identityValid()) return;
    try { await action("reference_adoption.review", { id: item.id, decision: "EXCLUDE", reviewer: reviewer.value, reason: reason.value }); }
    catch (error) { toast(error.message); }
  };
  card.append(form);
}

function appendBenchmarkPolicyReview(card, item) {
  const warning = document.createElement("div"); warning.className = "landmark-warning";
  warning.textContent = "This is a named production-policy decision. The template is editable and does not become approved until submitted by a reviewer.";
  card.append(warning);
  if (item.verification_error) {
    const error = document.createElement("p"); error.className = "policy-error"; error.textContent = item.verification_error; card.append(error);
  }
  const form = document.createElement("form"); form.className = "benchmark-policy-form";
  const strategy = document.createElement("textarea"); strategy.setAttribute("aria-label", "DGX foam LOD strategy JSON"); strategy.value = JSON.stringify(item.strategy_template, null, 2);
  const reviewer = document.createElement("input"); reviewer.placeholder = "Named DGX asset reviewer"; reviewer.required = true;
  const reason = document.createElement("input"); reason.placeholder = "Views and transition behavior reviewed"; reason.required = true;
  const submit = document.createElement("button"); submit.type = "submit"; submit.className = "approve"; submit.textContent = "Approve foam LOD policy";
  form.append(strategy, reviewer, reason, submit);
  form.onsubmit = async (event) => {
    event.preventDefault();
    try {
      const value = JSON.parse(strategy.value);
      await action("benchmark.review_dgx_foam_lod", { id: item.id, reviewer: reviewer.value, reason: reason.value, strategy: value });
    } catch (error) { toast(error.message); }
  };
  card.append(form);
}

function appendLandmarkReview(card, item) {
  const warning = document.createElement("div"); warning.className = "landmark-warning";
  warning.textContent = `${item.point_count} machine-proposed point(s). This review does not approve a camera.`;
  card.append(warning);
  if (item.known_limitations.length) {
    const limitations = document.createElement("ul"); limitations.className = "landmark-limitations";
    item.known_limitations.forEach((value) => { const row = document.createElement("li"); row.textContent = value; limitations.append(row); });
    card.append(limitations);
  }
  const decisionRows = [];
  item.views.forEach((view) => {
    const section = document.createElement("section"); section.className = "landmark-view";
    const heading = document.createElement("strong"); heading.textContent = `Reference ${view.reference_id.slice(0, 12)}`; section.append(heading);
    if (view.image_url && view.image_width && view.image_height) {
      const frame = document.createElement("div"); frame.className = "landmark-frame";
      const image = document.createElement("img"); image.src = view.image_url; image.alt = "Landmark review reference";
      const overlay = document.createElementNS("http://www.w3.org/2000/svg", "svg");
      overlay.setAttribute("viewBox", `0 0 ${view.image_width} ${view.image_height}`);
      overlay.setAttribute("aria-label", "Proposed landmark overlay");
      view.correspondences.forEach((point, index) => {
        const marker = document.createElementNS("http://www.w3.org/2000/svg", "circle");
        marker.setAttribute("cx", point.image_px[0]); marker.setAttribute("cy", point.image_px[1]);
        marker.setAttribute("r", Math.max(view.image_width, view.image_height) / 120);
        marker.setAttribute("class", "landmark-marker");
        const label = document.createElementNS("http://www.w3.org/2000/svg", "title"); label.textContent = `${index + 1}: ${point.landmark_id}`;
        marker.append(label); overlay.append(marker);
      });
      frame.append(image, overlay); section.append(frame);
    }
    const table = document.createElement("div"); table.className = "landmark-table";
    view.correspondences.forEach((point, index) => {
      const row = document.createElement("div"); row.className = "landmark-row";
      const label = document.createElement("label"); label.textContent = `${index + 1}. ${point.landmark_id} (${(point.confidence * 100).toFixed(0)}%)`;
      const decision = document.createElement("select"); decision.required = true;
      [["", "Choose decision"], ["accept", "Accept unchanged"], ["reject", "Reject"], ["correct", "Correct coordinates"]].forEach(([value, text]) => {
        const option = document.createElement("option"); option.value = value; option.textContent = text; decision.append(option);
      });
      const coordinates = document.createElement("div"); coordinates.className = "landmark-coordinates";
      const values = [...point.image_px, ...point.world_mm];
      const names = ["image x", "image y", "world x mm", "world y mm", "world z mm"];
      const inputs = values.map((value, valueIndex) => {
        const input = document.createElement("input"); input.type = "number"; input.step = "any"; input.value = value; input.disabled = true; input.setAttribute("aria-label", names[valueIndex]); coordinates.append(input); return input;
      });
      decision.onchange = () => { inputs.forEach((input) => { input.disabled = decision.value !== "correct"; input.required = decision.value === "correct"; }); };
      row.append(label, decision, coordinates); table.append(row);
      decisionRows.push({ referenceId: view.reference_id, landmarkId: point.landmark_id, decision, inputs });
    });
    section.append(table); card.append(section);
  });
  const form = document.createElement("form"); form.className = "landmark-review-form";
  const reviewer = document.createElement("input"); reviewer.placeholder = "Named landmark reviewer"; reviewer.required = true;
  const reason = document.createElement("input"); reason.placeholder = "Evidence-bound review reason"; reason.required = true;
  const submit = document.createElement("button"); submit.type = "submit"; submit.className = "approve"; submit.textContent = "Submit every point decision";
  form.append(reviewer, reason, submit);
  form.onsubmit = async (event) => {
    event.preventDefault();
    if (decisionRows.some((row) => !row.decision.value)) {
      toast("Choose accept, reject, or correct for every proposed landmark."); return;
    }
    try {
      const decisions = decisionRows.map((row) => {
        const value = { reference_id: row.referenceId, landmark_id: row.landmarkId, decision: row.decision.value };
        if (row.decision.value === "correct") {
          const numbers = row.inputs.map((input) => Number(input.value));
          if (numbers.some((number) => !Number.isFinite(number))) throw new Error("Corrected landmark coordinates must be finite numbers.");
          value.image_px = numbers.slice(0, 2); value.world_mm = numbers.slice(2);
        }
        return value;
      });
      await action("landmarks.review", { id: item.id, reviewer: reviewer.value, reason: reason.value, decisions });
    }
    catch (error) { toast(error.message); }
  };
  card.append(form);
}

function renderQueue(data) {
  const queue = $("#review-queue");
  queue.replaceChildren();
  $("#queue-count").textContent = data.review_queue.length;
  data.review_queue.forEach((item) => {
    const card = document.createElement("article");
    card.className = "queue-card";
    const header = document.createElement("header");
    const heading = document.createElement("div");
    const title = document.createElement("strong"); title.textContent = item.title;
    const detail = document.createElement("small"); detail.textContent = `${item.kind} · ${item.state}`;
    heading.append(title, document.createElement("br"), detail);
    const identifier = document.createElement("code"); identifier.textContent = item.id.slice(0, 12);
    header.append(heading, identifier);
    card.append(header);
    if (item.kind === "role_task") {
      const message = document.createElement("p"); message.textContent = item.waiting_reason || "Advisory role task is assigned and awaiting governed output.";
      const authority = document.createElement("small"); authority.textContent = item.authority;
      card.append(message, authority); queue.append(card); return;
    }
    if (item.kind === "reference_adoption") {
      appendReferenceAdoptionReview(card, item); queue.append(card); return;
    }
    if (item.kind === "benchmark_policy") {
      appendBenchmarkPolicyReview(card, item); queue.append(card); return;
    }
    if (item.kind === "landmark") {
      appendLandmarkReview(card, item); queue.append(card); return;
    }
    if (item.kind === "reference_mask") {
      const grid = document.createElement("div"); grid.className = "camera-evidence-grid";
      [[item.reference_image_url, "Immutable reference"], [item.mask_image_url, "Machine mask proposal"]].forEach(([url, label]) => {
        const figure = document.createElement("figure");
        if (url) { const image = document.createElement("img"); image.src = url; image.alt = label; figure.append(image); }
        const caption = document.createElement("figcaption"); caption.textContent = label; figure.append(caption); grid.append(figure);
      });
      const authority = document.createElement("div"); authority.className = "camera-authority nonmetric";
      authority.textContent = `${item.method} · ${item.confidence} proposal · ${item.authority}`;
      card.append(grid, authority);
    }
    if (item.kind === "camera") {
      const authority = document.createElement("div"); authority.className = item.metric_authority ? "camera-authority metric" : "camera-authority nonmetric";
      authority.textContent = item.metric_authority
        ? `Metric authority · ${item.camera_count} camera(s) · minimum confidence ${(item.minimum_confidence * 100).toFixed(0)}%`
        : `${item.registration_classes.join(", ")} · ${item.camera_count} camera(s) · ${item.authority_warning}`;
      card.append(authority);
      if (item.prioritization_note) {
        const note = document.createElement("div"); note.className = "camera-prioritization"; note.textContent = item.prioritization_note; card.append(note);
      }
      const coverage = document.createElement("div"); coverage.className = item.covers_acceptance_references ? "camera-coverage complete" : "camera-coverage incomplete";
      coverage.textContent = item.covers_acceptance_references ? "Covers every acceptance-eligible image." : "Does not cover every acceptance-eligible image.";
      card.append(coverage);
      const views = document.createElement("div"); views.className = "camera-evidence-grid";
      item.views.forEach((view) => {
        const figure = document.createElement("figure");
        if (view.image_url) { const image = document.createElement("img"); image.src = view.image_url; image.alt = view.original_name || "Camera source reference"; figure.append(image); }
        const caption = document.createElement("figcaption");
        const fit = view.search_silhouette_iou == null ? "fit not recorded" : `fit IoU ${Number(view.search_silhouette_iou).toFixed(4)}`;
        caption.textContent = `${view.viewpoint_label || view.original_name || view.reference_id} · ${(view.confidence * 100).toFixed(0)}% · ${fit}`;
        figure.append(caption); views.append(figure);
      });
      card.append(views);
    }
    if (item.kind === "repair") {
      if (item.render_url) {
        const image = document.createElement("img"); image.className = "repair-evidence-image"; image.src = item.render_url; image.alt = "Applied repair validation render"; card.append(image);
      }
      const details = document.createElement("details"); details.className = "repair-evidence-details";
      const summary = document.createElement("summary"); summary.textContent = "Expected change and validation evidence";
      const record = document.createElement("pre"); record.className = "record"; record.textContent = pretty({ expected: item.expected, validation: item.validation, acceptance: item.acceptance });
      details.append(summary, record); card.append(details);
    }
    if (item.kind === "repair" && item.state === "approved") {
      const controls = document.createElement("div"); controls.className = "job-actions";
      const apply = document.createElement("button"); apply.type = "button"; apply.className = "approve"; apply.textContent = "Run approved checkpoint";
      apply.onclick = async () => { try { await action("repair.apply", { id: item.id }); } catch (error) { toast(error.message); } };
      controls.append(apply); card.append(controls); queue.append(card); return;
    }
    const form = document.createElement("form");
    const reviewer = document.createElement("input"); reviewer.placeholder = "Named reviewer"; reviewer.required = true;
    const reason = document.createElement("input"); reason.placeholder = "Evidence-bound reason"; reason.required = true;
    const approve = document.createElement("button"); approve.type = "submit"; approve.className = "approve"; approve.textContent = item.kind === "repair" && item.state === "proposed" ? "Authorize evaluation" : "Approve";
    const reject = document.createElement("button"); reject.type = "button"; reject.className = "reject"; reject.textContent = "Reject";
    form.append(reviewer, reason, approve, reject);
    const send = async (accepted) => {
      const payload = { id: item.id, accepted, reviewer: reviewer.value, reason: reason.value };
      if (item.kind === "repair" && item.state === "proposed") delete payload.accepted;
      if (item.kind === "repair" && item.state === "applied" && accepted) {
        payload.receipt_id = data.latest_receipt?.id || null;
      }
      await action(actionName(item, accepted), payload);
    };
    form.onsubmit = async (event) => { event.preventDefault(); try { await send(true); } catch (error) { toast(error.message); } };
    reject.onclick = async () => { if (!form.reportValidity()) return; try { await send(false); } catch (error) { toast(error.message); } };
    if (item.kind === "fit" && !item.constraints_pass) {
      approve.disabled = true;
      approve.title = "Fit constraints must pass before application";
    }
    card.append(form); queue.append(card);
  });
  if (!data.review_queue.length) {
    const empty = document.createElement("div"); empty.className = "empty"; empty.textContent = "The named review queue is clear."; queue.append(empty);
  }
}

function renderJobs(data) {
  const root = $("#job-list");
  root.replaceChildren();
  $("#job-count").textContent = data.jobs.length;
  data.workers.forEach((worker) => {
    const card = document.createElement("article"); card.className = "queue-card";
    const header = document.createElement("header");
    const heading = document.createElement("div");
    const name = document.createElement("strong"); name.textContent = worker.name;
    const hardware = worker.capabilities.hardware.join(", ") || "hardware unspecified";
    const detail = document.createElement("small");
    detail.textContent = `${worker.worker_class} worker · ${worker.status} · ${hardware} · ${worker.capabilities.vram_gb} GB VRAM`;
    heading.append(name, document.createElement("br"), detail);
    const load = document.createElement("code");
    load.textContent = `${worker.load.current_jobs || 0} active`;
    header.append(heading, load); card.append(header); root.append(card);
  });
  data.jobs.forEach((job) => {
    const card = document.createElement("article"); card.className = "queue-card";
    const header = document.createElement("header");
    const heading = document.createElement("div");
    const operation = document.createElement("strong"); operation.textContent = job.operation;
    const detail = document.createElement("small"); detail.textContent = `${job.status} · ${job.created_at}`;
    heading.append(operation, document.createElement("br"), detail);
    const identifier = document.createElement("code"); identifier.textContent = job.id.slice(0, 12);
    header.append(heading, identifier); card.append(header);
    if (job.error) {
      const error = document.createElement("pre"); error.className = "record"; error.textContent = pretty(job.error); card.append(error);
    }
    if (["queued", "running"].includes(job.status)) {
      const controls = document.createElement("div"); controls.className = "job-actions";
      const cancel = document.createElement("button"); cancel.type = "button"; cancel.className = "reject"; cancel.textContent = "Request cancellation";
      cancel.onclick = async () => { try { await action("job.cancel", { id: job.id }); } catch (error) { toast(error.message); } };
      controls.append(cancel); card.append(controls);
    }
    root.append(card);
  });
  if (!data.jobs.length) {
    const empty = document.createElement("div"); empty.className = "empty"; empty.textContent = "No coordinator jobs recorded."; root.append(empty);
  }
}

function render(data) {
  renderHeader(data); renderComparison(data); renderGeometry(data); renderTrees(data); renderMeasurements(data); renderCameras(data); renderCoverage(data); renderAcceptance(data); renderQueue(data); renderJobs(data);
}

async function load() {
  const response = await fetch("/api/snapshot", { cache: "no-store" });
  if (!response.ok) throw new Error(`snapshot failed: ${response.status}`);
  state.snapshot = await response.json(); render(state.snapshot);
}

$$('.tab').forEach((tab) => tab.addEventListener("click", () => {
  $$('.tab').forEach((item) => item.classList.toggle("active", item === tab));
  $$('.panel').forEach((panel) => panel.classList.toggle("active", panel.id === tab.dataset.panel));
}));
$("#overlay-opacity").addEventListener("input", updateWipe);
$("#wipe-position").addEventListener("input", updateWipe);
$("#refresh").addEventListener("click", () => load().catch((error) => toast(error.message)));
$("#run-audit").addEventListener("click", () => action("project.audit").catch((error) => toast(error.message)));
$("#export-receipt").addEventListener("click", () => action("receipt.export").catch((error) => toast(error.message)));
updateWipe();
load().catch((error) => toast(error.message));
