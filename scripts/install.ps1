# Install the Merc supplier agent from a signed Windows release archive.
[CmdletBinding()]
param(
    [string]$Version = $env:MERC_AGENT_VERSION,
    [string]$BaseUrl = $env:MERC_AGENT_BASE_URL,
    [switch]$Start,
    [switch]$Check,
    [switch]$Uninstall,
    [switch]$Purge
)

$ErrorActionPreference = "Stop"
$Repo = if ($env:MERC_AGENT_REPO) { $env:MERC_AGENT_REPO } else { "joshuahickscorp/merc" }
$Root = Join-Path $env:LOCALAPPDATA "Merc"
$BinDir = Join-Path $Root "bin"
$Bin = Join-Path $BinDir "merc-agent.exe"
$State = Join-Path $env:APPDATA "Merc"
$Config = Join-Path $State "agent.toml"
$Prefs = Join-Path $State "agent.prefs.toml"
$TaskName = "Merc Agent"
$Identity = if ($env:MERC_AGENT_IDENTITY_REGEXP) { $env:MERC_AGENT_IDENTITY_REGEXP } else { "^https://github\.com/$([regex]::Escape($Repo))/\.github/workflows/agent-release\.yml@refs/(tags/agent-v.*|heads/main)$" }
$Issuer = if ($env:MERC_AGENT_OIDC_ISSUER) { $env:MERC_AGENT_OIDC_ISSUER } else { "https://token.actions.githubusercontent.com" }

function Say([string]$Message) { Write-Host "[install] $Message" -ForegroundColor Cyan }
function Fail([string]$Message) { throw "[install] $Message" }

if ($Uninstall) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $Bin -Force -ErrorAction SilentlyContinue
    if ($Purge) { Remove-Item -LiteralPath $State -Recurse -Force -ErrorAction SilentlyContinue }
    Say "removed the scheduled task and agent binary$(if ($Purge) { '; purged state' } else { '; kept config and state' })"
    exit 0
}

if (-not [Environment]::Is64BitOperatingSystem -or $env:PROCESSOR_ARCHITECTURE -notmatch "AMD64|x86_64") {
    Fail "the signed Windows prebuild currently supports Windows amd64 only"
}
if ($Check) {
    Say "would install the signed Windows amd64 binary to $Bin"
    Say "would write $Config if absent and register the per-user '$TaskName' logon task"
    exit 0
}

if (-not $Version) {
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest"
    if ($release.tag_name -notlike "agent-*") { Fail "latest release is not an agent release" }
    $Version = $release.tag_name.Substring(6)
}
$Name = "merc-agent_${Version}_windows_amd64.zip"
if (-not $BaseUrl) { $BaseUrl = "https://github.com/$Repo/releases/download/agent-$Version" }
$ArchiveUrl = "$($BaseUrl.TrimEnd('/'))/$Name"
$SumsUrl = "$($BaseUrl.TrimEnd('/'))/SHA256SUMS"
$Work = Join-Path ([IO.Path]::GetTempPath()) ("merc-agent-install-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $Work | Out-Null
try {
    $Archive = Join-Path $Work $Name
    $Sums = Join-Path $Work "SHA256SUMS"
    Invoke-WebRequest -UseBasicParsing -Uri $ArchiveUrl -OutFile $Archive
    Invoke-WebRequest -UseBasicParsing -Uri $SumsUrl -OutFile $Sums
    $line = Get-Content -LiteralPath $Sums | Where-Object { $_ -match "\s(\./)?$([regex]::Escape($Name))$" } | Select-Object -First 1
    if (-not $line) { Fail "SHA256SUMS has no entry for $Name" }
    $expected = ($line -split "\s+")[0].ToLowerInvariant()
    $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $Archive).Hash.ToLowerInvariant()
    if ($actual -ne $expected) { Fail "checksum mismatch for $Name" }

    $cosign = Get-Command cosign -ErrorAction SilentlyContinue
    if (-not $cosign -and $env:MERC_AGENT_ALLOW_UNVERIFIED -ne "1") {
        Fail "cosign is required to verify publisher identity; set MERC_AGENT_ALLOW_UNVERIFIED=1 only to accept digest-only verification"
    }
    if ($cosign) {
        foreach ($item in @(@($Sums, $SumsUrl), @($Archive, $ArchiveUrl))) {
            $bundle = "$($item[0]).cosign.bundle"
            Invoke-WebRequest -UseBasicParsing -Uri "$($item[1]).cosign.bundle" -OutFile $bundle
            & $cosign.Source verify-blob --bundle $bundle --certificate-identity-regexp $Identity --certificate-oidc-issuer $Issuer $item[0] | Out-Null
            if ($LASTEXITCODE -ne 0) { Fail "publisher signature verification failed for $([IO.Path]::GetFileName($item[0]))" }
        }
        Say "verified archive and checksum publisher identity"
    } else {
        Write-Warning "installing with digest-only verification by explicit operator opt-out"
    }

    $Expanded = Join-Path $Work "expanded"
    Expand-Archive -LiteralPath $Archive -DestinationPath $Expanded
    $Extracted = Get-ChildItem -LiteralPath $Expanded -Filter merc-agent.exe -Recurse | Select-Object -First 1
    if (-not $Extracted) { Fail "archive did not contain merc-agent.exe" }
    New-Item -ItemType Directory -Force -Path $BinDir, $State | Out-Null
    Copy-Item -LiteralPath $Extracted.FullName -Destination $Bin -Force

    if (-not (Test-Path -LiteralPath $Config)) {
        $ControlUrl = if ($env:MERC_CONTROL_URL) { $env:MERC_CONTROL_URL } else { "http://localhost:8080" }
        $WorkerToken = if ($env:MERC_WORKER_TOKEN) { $env:MERC_WORKER_TOKEN } else { "PASTE_WORKER_TOKEN" }
        if ($ControlUrl -match '["\r\n]' -or $WorkerToken -match '["\r\n]') { Fail "control URL and worker token cannot contain quotes or newlines" }
        $configText = @"
control_url = "$ControlUrl"
worker_token = "$WorkerToken"
supplier_id = "00000000-0000-0000-0000-000000000000"
power_only = true
min_payout_usd_per_hr = 0.05
memory_headroom_gb = 8.0
max_memory_pct = 85.0
data_dir = "$($State.Replace('\','/'))/data"
"@
        [IO.File]::WriteAllText($Config, $configText, (New-Object Text.UTF8Encoding($false)))
    }
    if (-not (Test-Path -LiteralPath $Prefs)) {
        $prefsText = @"
paused = false
allowed_weekdays = [0, 1, 2, 3, 4, 5, 6]
allowed_workload_classes = ["embed", "batch_infer"]
allow_model_downloads = true
max_model_cache_gb = 4.0
max_bandwidth_mbps = 25.0
max_cpu_pct = 80.0
thermal_limit = "serious"
power_only = true
quiet_hours = [22, 6]
min_payout_usd_per_hr = 0.05
memory_headroom_gb = 8.0
max_memory_pct = 85.0
"@
        [IO.File]::WriteAllText($Prefs, $prefsText, (New-Object Text.UTF8Encoding($false)))
    }

    $action = New-ScheduledTaskAction -Execute $Bin -Argument "run --config `"$Config`""
    $trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
    $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -AllowStartIfOnBatteries
    Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings -Description "Merc supplier agent" -Force | Out-Null
    if ($Start) { Start-ScheduledTask -TaskName $TaskName }
    Say "installed $Bin and registered the per-user '$TaskName' task"
    if (-not $Start) { Say "start with: Start-ScheduledTask -TaskName '$TaskName' (or pass -Start)" }
} finally {
    Remove-Item -LiteralPath $Work -Recurse -Force -ErrorAction SilentlyContinue
}
