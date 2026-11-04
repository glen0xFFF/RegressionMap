<#
Start a RegressionMap analysis from CI, wait for it, and print a markdown
summary. Windows PowerShell 5.1 or PowerShell 7. Same inputs, output and exit
codes as scripts/ci-analyze.sh.

  $env:REGRESSIONMAP_URL = 'https://regressionmap.example.com'
  $env:REGRESSIONMAP_KEY = '...'   # an "analyze" key, from a CI secret
  $env:REPO = 'my-repo'; $env:BASELINE = '<good sha>'
  powershell -NoProfile -ExecutionPolicy Bypass -File scripts\ci-analyze.ps1

Environment:
  REGRESSIONMAP_URL  required. https://, or http:// for 127.0.0.1/localhost/[::1] only.
  REGRESSIONMAP_KEY  required. Read from the environment only, never printed.
  REPO, BASELINE     required.
  COMMAND_ID         default node-test.
  NOTIFY_URL         optional https:// webhook. Treat it as a secret: never printed.
  NOTIFY_FORMAT      optional: json (server default) or slack.
  TIMEOUT_SECONDS    default 600.
  POLL_SECONDS       default 3.

The summary goes to stdout and is appended to $env:GITHUB_STEP_SUMMARY when set.

Exit codes: 0 done, 1 usage error, 2 analysis error, 3 timeout,
4 HTTP/network/auth error (the API error code and message are printed).
#>
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
Set-StrictMode -Version 2

$Dash = [string][char]0x2014
$Ellipsis = [string][char]0x2026
try { [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false } catch { }

function Say([string]$m) { [Console]::Error.WriteLine('ci-analyze: ' + $m) }
function Exit-Usage([string]$m) { Say $m; exit 1 }

$base = [string]$env:REGRESSIONMAP_URL
if (-not $base) { Exit-Usage 'REGRESSIONMAP_URL is required' }
if (-not $env:REGRESSIONMAP_KEY) { Exit-Usage 'REGRESSIONMAP_KEY is required (set it from a CI secret)' }
if (-not $env:REPO) { Exit-Usage 'REPO is required' }
if (-not $env:BASELINE) { Exit-Usage 'BASELINE is required' }
$repo = [string]$env:REPO
$baseline = [string]$env:BASELINE
$commandId = if ($env:COMMAND_ID) { [string]$env:COMMAND_ID } else { 'node-test' }
$timeoutText = if ($env:TIMEOUT_SECONDS) { [string]$env:TIMEOUT_SECONDS } else { '600' }
$pollText = if ($env:POLL_SECONDS) { [string]$env:POLL_SECONDS } else { '3' }
if ($timeoutText -notmatch '^[0-9]+$') { Exit-Usage 'TIMEOUT_SECONDS must be a whole number' }
if ($pollText -notmatch '^[0-9]+$' -or [int]$pollText -eq 0) { Exit-Usage 'POLL_SECONDS must be a whole number above 0' }
$timeoutSeconds = [int]$timeoutText
$pollSeconds = [int]$pollText

$base = $base.TrimEnd('/')
if ($base -notmatch '^https://' -and $base -notmatch '^http://(127\.0\.0\.1|localhost|\[::1\])(:[0-9]+)?(/.*)?$') {
  Exit-Usage 'REGRESSIONMAP_URL must start with https:// (plain http:// is accepted for 127.0.0.1, localhost and [::1] only)'
}

# The key stays in this process; it is removed from the environment so nothing
# started later inherits it.
$key = [string]$env:REGRESSIONMAP_KEY
Remove-Item Env:REGRESSIONMAP_KEY -ErrorAction SilentlyContinue
$notifyUrl = [string]$env:NOTIFY_URL
Remove-Item Env:NOTIFY_URL -ErrorAction SilentlyContinue
$secrets = @($key, $notifyUrl) | Where-Object { $_ }
$headers = @{ Authorization = 'Bearer ' + $key; Accept = 'application/json' }

try {
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch { }

function Redact([string]$s) {
  foreach ($x in $secrets) { $s = $s.Replace($x, '[redacted]') }
  return $s
}
# Model output is untrusted text: one line, no HTML, no code spans, bounded length.
function Safe($s, [int]$max = 300) {
  if ($null -eq $s) { return '' }
  $t = Redact ([string]$s)
  $t = ($t -replace '\s+', ' ').Trim()
  $t = $t.Replace('&', '&amp;').Replace('<', '&lt;').Replace('>', '&gt;').Replace('`', "'")
  if ($t.Length -gt $max) { $t = $t.Substring(0, $max - 1) + $Ellipsis }
  return $t
}
# Property of a parsed JSON object, or $null when absent (StrictMode-safe).
function P($o, [string]$name) {
  if ($null -eq $o) { return $null }
  $prop = $o.PSObject.Properties[$name]
  if ($null -eq $prop) { return $null }
  return $prop.Value
}
function Emit([string]$text) {
  Write-Output $text
  if ($env:GITHUB_STEP_SUMMARY) {
    [IO.File]::AppendAllText($env:GITHUB_STEP_SUMMARY, $text + "`n", (New-Object System.Text.UTF8Encoding $false))
  }
}

# Calls the API. Returns the parsed body on 2xx; on any other outcome prints the
# API error code and message (never the key) and exits 4.
function Invoke-Api([string]$Method, [string]$Path, [string]$Body) {
  $p = @{ Uri = $base + $Path; Method = $Method; Headers = $headers; TimeoutSec = 30;
          UseBasicParsing = $true; MaximumRedirection = 0 }
  if ($Body) { $p.Body = [Text.Encoding]::UTF8.GetBytes($Body); $p.ContentType = 'application/json' }
  try {
    return Invoke-RestMethod @p
  } catch {
    $resp = $null
    if ($_.Exception.PSObject.Properties['Response']) { $resp = $_.Exception.Response }
    if ($null -eq $resp) {
      Say ("$Method $Path failed: " + (Safe $_.Exception.Message 300))
      exit 4
    }
    $code = [int]$resp.StatusCode
    $text = ''
    if ($_.ErrorDetails -and $_.ErrorDetails.Message) {
      $text = $_.ErrorDetails.Message
    } elseif ($resp -is [System.Net.HttpWebResponse]) {
      try { $text = (New-Object IO.StreamReader($resp.GetResponseStream())).ReadToEnd() } catch { }
    }
    $err = $null
    try { $err = P ($text | ConvertFrom-Json) 'error' } catch { }
    $errCode = P $err 'code'; if (-not $errCode) { $errCode = 'error' }
    $line = "HTTP $code " + (Safe $errCode 80)
    $msg = P $err 'message'
    if ($msg) { $line += ': ' + (Safe $msg) }
    Say $line
    if ($code -eq 429) {
      $ra = ''
      try {
        if ($resp.Headers -is [System.Net.WebHeaderCollection]) {
          $ra = [string]$resp.Headers['Retry-After']
        } else {
          $vals = $null
          if ($resp.Headers.TryGetValues('Retry-After', [ref]$vals)) { $ra = ($vals -join ',') }
        }
      } catch { }
      if ($ra) { Say ('Retry-After: ' + (Safe $ra 40) + ' s') } else { Say 'Retry-After: not given' }
    }
    exit 4
  }
}

$req = [ordered]@{ repo = $repo; baseline = $baseline; command_id = $commandId }
if ($notifyUrl) {
  $req.notify_url = $notifyUrl
  if ($env:NOTIFY_FORMAT) { $req.notify_format = [string]$env:NOTIFY_FORMAT }
}
$started = Invoke-Api 'POST' '/api/analyses' ($req | ConvertTo-Json -Compress)
$id = [string](P $started 'id')
if ($id -cnotmatch '^a[0-9a-z-]{1,40}$') { Say 'the server returned no valid analysis id'; exit 4 }
Say "started analysis $id of $repo (baseline $baseline, command $commandId)"

$clock = [Diagnostics.Stopwatch]::StartNew()
while ($true) {
  Start-Sleep -Seconds $pollSeconds
  $a = Invoke-Api 'GET' ('/api/analyses/' + $id) $null
  $status = [string](P $a 'status')
  if (@('running', 'done', 'error') -notcontains $status) { Say 'unexpected analysis status from the server'; exit 4 }
  if ($status -ne 'running') { break }
  if ($clock.Elapsed.TotalSeconds -ge $timeoutSeconds) {
    Say "timed out after ${timeoutSeconds}s waiting for analysis $id (it keeps running on the server)"
    Emit ("RegressionMap: timed out after ${timeoutSeconds}s waiting for analysis ``$id``.")
    exit 3
  }
}

function RunText($r) {
  if ($null -eq $r) { return 'not run' }
  if (P $r 'timed_out') { return 'FAIL (timed out)' }
  $ec = P $r 'exit_code'
  $word = if ($ec -eq 0) { 'PASS' } else { 'FAIL' }
  return "$word (exit $ec)"
}
function Short($s) { $t = [string]$s; if ($t.Length -gt 7) { $t = $t.Substring(0, 7) }; return (Safe $t 7) }
function Ids($xs) { return ((@($xs) | Where-Object { $null -ne $_ } | ForEach-Object { Safe $_ 12 }) -join ', ') }
function Loc($h) {
  $f = Safe (P $h 'file') 200
  if (-not $f) { return '' }
  $ls = [int](P $h 'line_start'); $le = [int](P $h 'line_end')
  if ($ls -le 0) { return '`' + $f + '`' }
  if ($le -gt $ls) { return '`' + $f + ':' + $ls + '-' + $le + '`' }
  return '`' + $f + ':' + $ls + '`'
}

$o = New-Object System.Collections.Generic.List[string]
$o.Add('## RegressionMap analysis'); $o.Add('')
$o.Add('- Repository: `' + (Safe (P $a 'repo') 100) + '`')
$o.Add('- Analysis: `' + (Safe (P $a 'id') 60) + '`')
$o.Add('- Status: **' + (Safe $status 20) + '**')
if ($status -eq 'error') { $o.Add('- Error: ' + (Safe (P $a 'error') 500)) }
$o.Add('- Baseline `' + (Short (P $a 'baseline')) + '`: ' + (RunText (P $a 'baseline_run')))
$o.Add('- HEAD `' + (Short (P $a 'current')) + '`: ' + (RunText (P $a 'current_run')))
$m = P $a 'model'
if ($m) {
  $line = '- Model: ' + (Safe (P $m 'status') 20)
  if (P $m 'model_id') { $line += ' (`' + (Safe (P $m 'model_id') 60) + '`)' }
  if (P $m 'message') { $line += " $Dash " + (Safe (P $m 'message')) }
  $o.Add($line)
}
$n = P $a 'notification'
if ($n) {
  $line = '- Notification: ' + (Safe (P $n 'status') 20) + ' to ' + (Safe (P $n 'host') 120) + ' (' + (Safe (P $n 'format') 10) +
          ', ' + [int](P $n 'attempts') + ' attempt(s)'
  if (P $n 'last_error') { $line += ', ' + (Safe (P $n 'last_error') 200) }
  $o.Add($line + ')')
}
if ($m) {
  $o.Add(''); $o.Add("### AI-generated hypotheses $Dash not verified"); $o.Add('')
  $o.Add('The test runner decides PASS or FAIL. These are ranked guesses that cite evidence IDs from the analysis; check them before acting.')
  $o.Add('')
  $hs = @()
  $result = P $m 'result'
  if ((P $m 'status') -eq 'ok' -and $result) { $hs = @(P $result 'hypotheses') | Where-Object { $null -ne $_ } }
  $hs = @($hs)
  if ($hs.Count -eq 0) { $o.Add('No valid hypotheses.') }
  foreach ($h in $hs) {
    $line = [string][int](P $h 'rank') + '. **' + (Safe (P $h 'title') 200) + '**'
    $l = Loc $h
    if ($l) { $line += " $Dash " + $l }
    $line += " $Dash confidence: " + (Safe (P $h 'confidence_label') 10)
    $sup = Ids (P $h 'supporting_evidence_ids'); if (-not $sup) { $sup = 'none' }
    $line += " $Dash evidence: " + $sup
    $ctr = Ids (P $h 'counter_evidence_ids')
    if ($ctr) { $line += " $Dash counter-evidence: " + $ctr }
    $o.Add($line)
    if (P $h 'reason') { $o.Add('   ' + (Safe (P $h 'reason') 500)) }
  }
  $o.Add(''); $o.Add('Rejected by validation: ' + @(P $m 'rejected' | Where-Object { $null -ne $_ }).Count)
}
Emit ($o -join "`n")
if ($status -eq 'done') { exit 0 }
exit 2
