<#
Start a RegressionMap analysis from CI, wait for it, and print a markdown
summary. Windows PowerShell 5.1 or PowerShell 7. Same inputs, output and exit
codes as scripts/ci-analyze.sh.

  $env:REGRESSIONMAP_URL = 'https://regressionmap.example.com'
  $env:REGRESSIONMAP_KEY = '...'   # an "analyze" key, from a CI secret
  $env:REPO = 'my-repo'; $env:BASELINE = '<good sha>'
  powershell -NoProfile -ExecutionPolicy Bypass -File scripts\ci-analyze.ps1

Environment:
  REGRESSIONMAP_URL  required. https://, or http:// for 127.0.0.1, localhost or
                     [::1] only (optionally :port). No user information ('@')
                     and no backslash before the path.
  REGRESSIONMAP_KEY  required. Read from the environment only, never printed.
  REPO, BASELINE     required.
  COMMAND_ID         default node-test.
  NOTIFY_URL         optional https:// webhook. Treat it as a secret: never printed.
  NOTIFY_FORMAT      optional: json (server default) or slack.
  TIMEOUT_SECONDS    default 600; digits, no leading zero.
  POLL_SECONDS       default 3; digits above 0, no leading zero.
  GITHUB_SHA         set by GitHub Actions. When the analysed HEAD differs, the
                     summary warns that the server's clone may lag behind CI.
  GITHUB_ACTIONS     set by GitHub Actions. When it is "true", output that
                     carries server data is fenced with a random stop-commands
                     token.

The summary goes to stdout and is appended to $env:GITHUB_STEP_SUMMARY when set.
Server text is untrusted: control characters are removed, '::' and '##[' are
broken, and in the summary links, images and HTML are escaped. Response bodies
are decoded as UTF-8 from the raw bytes (the server sends no charset, which
Windows PowerShell 5.1 would otherwise read as ISO-8859-1).

Exit codes: 0 done, 1 usage error, 2 analysis error, 3 timeout,
4 HTTP/network/auth error (the API error code and message are printed).
#>
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
Set-StrictMode -Version 2

$Dash = [string][char]0x2014
$Ellipsis = [string][char]0x2026
$Zwsp = [string][char]0x200B
$Utf8 = New-Object System.Text.UTF8Encoding $false
try { [Console]::OutputEncoding = $Utf8 } catch { }

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
# No leading zeros, as in the sh script (where 010 would be octal).
if ($timeoutText -cnotmatch '^(0|[1-9][0-9]*)\z') { Exit-Usage 'TIMEOUT_SECONDS must be a whole number of seconds, without leading zeros' }
if ($pollText -cnotmatch '^[1-9][0-9]*\z') { Exit-Usage 'POLL_SECONDS must be a whole number above 0, without leading zeros' }
$timeoutSeconds = [int]$timeoutText
$pollSeconds = [int]$pollText

# The URL rule (the same as in ci-analyze.sh): https:// to any host, or plain
# http:// when the authority is exactly 127.0.0.1, localhost or [::1] with an
# optional :port. The authority (up to the first '/') must not hold '@' (user
# information, which would move the real host after it) or a backslash.
$urlRule = 'REGRESSIONMAP_URL must start with https:// (plain http:// is accepted for 127.0.0.1, localhost and [::1] only)'
$base = $base.TrimEnd('/')
if ($base.StartsWith('https://', [StringComparison]::Ordinal)) {
  $authority = $base.Substring(8); $plainHttp = $false
} elseif ($base.StartsWith('http://', [StringComparison]::Ordinal)) {
  $authority = $base.Substring(7); $plainHttp = $true
} else {
  Exit-Usage $urlRule
}
$slash = $authority.IndexOf('/')
if ($slash -ge 0) { $authority = $authority.Substring(0, $slash) }
if (-not $authority) { Exit-Usage 'REGRESSIONMAP_URL has no host' }
if ($authority.Contains('@') -or $authority.Contains('\')) { Exit-Usage "REGRESSIONMAP_URL must not contain '@' or a backslash before its path" }
if ($plainHttp -and $authority -cnotmatch '^(127\.0\.0\.1|localhost|\[::1\])(:[0-9]+)?\z') { Exit-Usage $urlRule }

# The key stays in this process; it is removed from the environment so nothing
# started later inherits it.
$key = [string]$env:REGRESSIONMAP_KEY
Remove-Item Env:REGRESSIONMAP_KEY -ErrorAction SilentlyContinue
$notifyUrl = [string]$env:NOTIFY_URL
Remove-Item Env:NOTIFY_URL -ErrorAction SilentlyContinue
$secrets = @($key, $notifyUrl) | Where-Object { $_ }

try {
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
} catch { }
try { [Net.ServicePointManager]::Expect100Continue = $false } catch { }

# In GitHub Actions, every block of output that carries server data is fenced
# with a random token, so the runner reads no workflow command in it.
$Token = ''
if ($env:GITHUB_ACTIONS -ceq 'true') { $Token = [guid]::NewGuid().ToString('N') }
function Write-Untrusted([IO.TextWriter]$w, [string]$text) {
  if ($Token) { $w.WriteLine('::stop-commands::' + $Token) }
  $w.WriteLine($text)
  if ($Token) { $w.WriteLine('::' + $Token + '::') }
}
function Say-Untrusted([string]$m) { Write-Untrusted ([Console]::Error) ('ci-analyze: ' + $m) }

# Server data is untrusted. Text fields are printed only when they are
# strings, numbers only when they are safe integers ('?' otherwise), and
# objects only when they are plain objects.
function Redact([string]$s) {
  foreach ($x in $secrets) { $s = $s.Replace($x, '[redacted]') }
  return $s
}
# At most $max characters (code points), the last one an ellipsis when cut.
function Cut([string]$t, [int]$max) {
  $n = 0; $i = 0; $keep = 0
  while ($i -lt $t.Length) {
    if ($n -eq $max - 1) { $keep = $i }
    if ([char]::IsHighSurrogate($t[$i]) -and $i + 1 -lt $t.Length -and [char]::IsLowSurrogate($t[$i + 1])) { $i += 2 } else { $i += 1 }
    $n++
    if ($n -gt $max) { return $t.Substring(0, $keep) + $Ellipsis }
  }
  return $t
}
# One plain line: secrets redacted, whitespace collapsed, control characters
# (U+0000-U+001F, U+007F-U+009F) removed, at most $max characters, and '::'
# and '##[' broken with a zero-width space so that the Actions runner never
# reads a workflow command in it. For stderr.
function Plain-Text($s, [int]$max = 300) {
  if ($s -isnot [string]) { return '' }
  # .NET's \s lacks U+FEFF and JavaScript's lacks U+0085: both scripts use
  # the union, so they collapse the same characters.
  $t = [regex]::Replace((Redact $s), ('[\s' + [char]0xFEFF + ']+'), ' ')
  $t = [regex]::Replace($t, '[\u0000-\u001f\u007f-\u009f]', '')
  $t = Redact ([regex]::Replace($t, ' {2,}', ' ').Trim(' '))
  $t = Cut $t $max
  $t = [regex]::Replace($t, ':(?=:)', ':' + $Zwsp)
  return [regex]::Replace($t, '#(?=#\[)', '#' + $Zwsp)
}
# Inside a code span nothing is interpreted; only a backtick would end it.
function Code-Text($s, [int]$max = 300) { return (Plain-Text $s $max).Replace('`', "'") }
# Inline Markdown: no links, images, HTML or code spans.
function Md-Text($s, [int]$max = 300) {
  $t = [regex]::Replace((Plain-Text $s $max), '[\\\[\]]', '\$0')
  return $t.Replace('&', '&amp;').Replace('<', '&lt;').Replace('>', '&gt;').Replace('`', "'")
}
# A JSON integer within +-(2^53-1) as text, else '?'.
function Int-Text($v) {
  if (-not ($v -is [int] -or $v -is [long] -or $v -is [decimal] -or $v -is [double])) { return '?' }
  if ($v -is [double] -and ([double]::IsNaN($v) -or [double]::IsInfinity($v))) { return '?' }
  if ([Math]::Abs([double]$v) -gt 9007199254740991) { return '?' }
  if ([Math]::Truncate($v) -ne $v) { return '?' }
  return ([long]$v).ToString([Globalization.CultureInfo]::InvariantCulture)
}
# A parsed JSON object, or $null for anything else.
function O($x) {
  if ($x -is [Management.Automation.PSCustomObject]) { return $x }
  return $null
}
# Property of a parsed JSON object (exact name), or $null (StrictMode-safe).
# The comma keeps an array value from being unrolled.
function P($o, [string]$name) {
  if ($o -isnot [Management.Automation.PSCustomObject]) { return $null }
  $prop = $o.PSObject.Properties[$name]
  if ($null -eq $prop -or $prop.Name -cne $name) { return $null }
  return , $prop.Value
}
function Read-Json([string]$text) {
  try { return , ($text | ConvertFrom-Json) } catch { return $null }
}
function Emit([string]$text) {
  Write-Untrusted ([Console]::Out) $text
  if ($env:GITHUB_STEP_SUMMARY) {
    [IO.File]::AppendAllText($env:GITHUB_STEP_SUMMARY, $text + "`n", $Utf8)
  }
}

# One HTTP request, redirects not followed. Returns @{ Code; Text; RetryAfter }
# with the body decoded as UTF-8 from the raw bytes, for every status; or
# $null after a network failure, with the reason in $script:NetError.
function Send-Request([string]$Method, [string]$Path, [string]$Body) {
  $script:NetError = ''
  $resp = $null
  try {
    $req = [Net.WebRequest]::Create($base + $Path)
    $req.Method = $Method
    $req.Timeout = 30000
    $req.ReadWriteTimeout = 30000
    $req.AllowAutoRedirect = $false
    $req.Accept = 'application/json'
    $req.Headers['Authorization'] = 'Bearer ' + $key
    if ($Body) {
      $bytes = $Utf8.GetBytes($Body)
      $req.ContentType = 'application/json'
      $req.ContentLength = $bytes.Length
      $stream = $req.GetRequestStream()
      try { $stream.Write($bytes, 0, $bytes.Length) } finally { $stream.Close() }
    }
    $resp = $req.GetResponse()
  } catch {
    $ex = $_.Exception
    $web = $ex
    while ($null -ne $web -and -not ($web -is [Net.WebException] -and $null -ne $web.Response)) { $web = $web.InnerException }
    if ($null -eq $web) {
      if ($ex -is [Management.Automation.MethodInvocationException] -and $null -ne $ex.InnerException) { $ex = $ex.InnerException }
      $script:NetError = $ex.Message
      return $null
    }
    $resp = $web.Response
  }
  try {
    $buffer = New-Object IO.MemoryStream
    $stream = $resp.GetResponseStream()
    if ($null -ne $stream) { $stream.CopyTo($buffer) }
    return @{
      Code       = [int]$resp.StatusCode
      Text       = $Utf8.GetString($buffer.ToArray())
      RetryAfter = [string]$resp.Headers['Retry-After']
    }
  } catch {
    $ex = $_.Exception
    if ($ex -is [Management.Automation.MethodInvocationException] -and $null -ne $ex.InnerException) { $ex = $ex.InnerException }
    $script:NetError = $ex.Message
    return $null
  } finally {
    $resp.Close()
  }
}

# The API error of a non-2xx response as plain-text lines for stderr.
function Write-HttpError($r) {
  $err = O (P (O (Read-Json $r.Text)) 'error')
  $errCode = Plain-Text (P $err 'code') 80
  if (-not $errCode) { $errCode = 'error' }
  $line = 'ci-analyze: HTTP ' + $r.Code + ' ' + $errCode
  $msg = Plain-Text (P $err 'message')
  if ($msg) { $line += ': ' + $msg }
  if ($r.Code -eq 429) {
    $ra = [string]$r.RetryAfter
    if ($ra -cmatch '^[0-9]{1,15}\z') { $ra = $ra + ' s' } else { $ra = Plain-Text $ra 40 }
    if (-not $ra) { $ra = 'not given' }
    $line += "`n" + 'ci-analyze: Retry-After: ' + $ra
  }
  Write-Untrusted ([Console]::Error) $line
}

# Calls the API. Returns the parsed body of a 2xx response; on any other
# outcome prints the HTTP status and the API error (never the key) and exits 4.
function Invoke-Api([string]$Method, [string]$Path, [string]$Body) {
  $r = Send-Request $Method $Path $Body
  if ($null -eq $r) {
    Say-Untrusted ("$Method $Path failed: " + (Plain-Text $script:NetError 300))
    exit 4
  }
  if ($r.Code -lt 200 -or $r.Code -gt 299) { Write-HttpError $r; exit 4 }
  return , (Read-Json $r.Text)
}
function Is-Pending($a) { return ((P (O (P $a 'notification')) 'status') -ceq 'pending') }

$req = [ordered]@{ repo = $repo; baseline = $baseline; command_id = $commandId }
if ($notifyUrl) {
  $req.notify_url = $notifyUrl
  if ($env:NOTIFY_FORMAT) { $req.notify_format = [string]$env:NOTIFY_FORMAT }
}
$started = Invoke-Api 'POST' '/api/analyses' ($req | ConvertTo-Json -Compress)
$id = P $started 'id'
if ($id -isnot [string] -or $id -cnotmatch '^a[0-9a-z-]{1,40}\z') { Say 'the server returned no valid analysis id'; exit 4 }
Say-Untrusted "started analysis $id of $repo (baseline $baseline, command $commandId)"

$validStatus = @('running', 'done', 'error')
$clock = [Diagnostics.Stopwatch]::StartNew()
while ($true) {
  Start-Sleep -Seconds $pollSeconds
  $a = Invoke-Api 'GET' ('/api/analyses/' + $id) $null
  $status = P $a 'status'
  if ($status -isnot [string] -or $validStatus -cnotcontains $status) { Say 'unexpected analysis status from the server'; exit 4 }
  if ($status -ne 'running') { break }
  if ($clock.Elapsed.TotalSeconds -ge $timeoutSeconds) {
    Say-Untrusted "timed out after ${timeoutSeconds}s waiting for analysis $id (it keeps running on the server)"
    Emit ("RegressionMap: timed out after ${timeoutSeconds}s waiting for analysis ``$id``.")
    exit 3
  }
}

# The notification is sent when the analysis finishes, so its result often
# lands a few seconds later. Wait up to 30 s more for it, every 2 s. A failed
# GET here ends the wait but not the run: the analysis result is known.
if (Is-Pending $a) {
  Say 'the analysis finished; waiting up to 30s for its notification to be delivered'
  $wait = [Diagnostics.Stopwatch]::StartNew()
  while ($wait.Elapsed.TotalSeconds -lt 30) {
    Start-Sleep -Seconds 2
    $r = Send-Request 'GET' ('/api/analyses/' + $id) $null
    if ($null -eq $r) { Say-Untrusted ('stopped waiting for the notification: ' + (Plain-Text $script:NetError 300)); break }
    if ($r.Code -ne 200) { Say ('stopped waiting for the notification: HTTP ' + $r.Code); break }
    $next = Read-Json $r.Text
    $nextStatus = P $next 'status'
    if ($nextStatus -isnot [string] -or $validStatus -cnotcontains $nextStatus) {
      Say 'stopped waiting for the notification: unexpected analysis status from the server'; break
    }
    $a = $next
    if (-not (Is-Pending $a)) { break }
  }
}

function RunText($r) {
  $r = O $r
  if ($null -eq $r) { return 'not run' }
  $to = P $r 'timed_out'
  if ($to -is [bool] -and $to) { return 'FAIL (timed out)' }
  $ec = Int-Text (P $r 'exit_code')
  $word = if ($ec -ceq '0') { 'PASS' } else { 'FAIL' }
  return "$word (exit $ec)"
}
function Short($s) {
  $t = if ($s -is [string]) { $s } else { '' }
  if ($t.Length -gt 7) { $t = $t.Substring(0, 7) }
  return (Code-Text $t 7)
}
function Ids($xs) {
  if ($xs -isnot [array]) { return '' }
  $parts = @(foreach ($x in $xs) { if ($x -is [string]) { $t = Md-Text $x 12; if ($t) { $t } } })
  return ($parts -join ', ')
}
function Loc($h) {
  $f = Code-Text (P $h 'file') 200
  if (-not $f) { return '' }
  $ls = Int-Text (P $h 'line_start')
  if ($ls -ceq '?') { return '`' + $f + ':?`' }
  if ([long]$ls -le 0) { return '`' + $f + '`' }
  $le = Int-Text (P $h 'line_end')
  if ($le -ceq '?') { return '`' + $f + ':' + $ls + '-?`' }
  if ([long]$le -gt [long]$ls) { return '`' + $f + ':' + $ls + '-' + $le + '`' }
  return '`' + $f + ':' + $ls + '`'
}

$o = New-Object System.Collections.Generic.List[string]
$o.Add('## RegressionMap analysis'); $o.Add('')
$o.Add('- Repository: `' + (Code-Text (P $a 'repo') 100) + '`')
$o.Add('- Analysis: `' + (Code-Text (P $a 'id') 60) + '`')
$o.Add('- Status: **' + (Md-Text $status 20) + '**')
if ($status -ceq 'error') { $o.Add('- Error: ' + (Md-Text (P $a 'error') 500)) }
$o.Add('- Baseline `' + (Short (P $a 'baseline')) + '`: ' + (RunText (P $a 'baseline_run')))
$o.Add('- HEAD `' + (Short (P $a 'current')) + '`: ' + (RunText (P $a 'current_run')))
# The server tests its own clone: say so when that is not the commit CI ran.
$want = ([string]$env:GITHUB_SHA).Trim().ToLowerInvariant()
$current = P $a 'current'
$got = if ($current -is [string]) { $current.Trim().ToLowerInvariant() } else { '' }
if ($want -and $got) {
  $k = [Math]::Min($want.Length, $got.Length)
  if ($k -lt 7 -or $want.Substring(0, $k) -cne $got.Substring(0, $k)) {
    $o.Add('- Warning: the server analysed `' + (Short $got) + '`, not `' + (Short $want) + '` (GITHUB_SHA): the server''s clone may lag behind CI.')
  }
}
$m = O (P $a 'model')
if ($m) {
  $line = '- Model: ' + (Md-Text (P $m 'status') 20)
  $mid = Code-Text (P $m 'model_id') 60
  if ($mid) { $line += ' (`' + $mid + '`)' }
  $msg = Md-Text (P $m 'message')
  if ($msg) { $line += " $Dash " + $msg }
  $o.Add($line)
}
$n = O (P $a 'notification')
if ($n) {
  $st = if ((P $n 'status') -ceq 'pending') { 'still pending' } else { Md-Text (P $n 'status') 20 }
  $line = '- Notification: ' + $st + ' to ' + (Md-Text (P $n 'host') 120) + ' (' + (Md-Text (P $n 'format') 10) +
          ', ' + (Int-Text (P $n 'attempts')) + ' attempt(s)'
  $lastError = Md-Text (P $n 'last_error') 200
  if ($lastError) { $line += ', ' + $lastError }
  $o.Add($line + ')')
}
if ($m) {
  $o.Add(''); $o.Add("### AI-generated hypotheses $Dash not verified"); $o.Add('')
  $o.Add('The test runner decides PASS or FAIL. These are ranked guesses that cite evidence IDs from the analysis; check them before acting.')
  $o.Add('')
  $hs = @()
  $result = O (P $m 'result')
  $list = P $result 'hypotheses'
  if ((P $m 'status') -ceq 'ok' -and $list -is [array]) { $hs = @(foreach ($h in $list) { if (O $h) { $h } }) }
  if ($hs.Count -eq 0) { $o.Add('No valid hypotheses.') }
  foreach ($h in $hs) {
    $line = (Int-Text (P $h 'rank')) + '. **' + (Md-Text (P $h 'title') 200) + '**'
    $l = Loc $h
    if ($l) { $line += " $Dash " + $l }
    $line += " $Dash confidence: " + (Md-Text (P $h 'confidence_label') 10)
    $sup = Ids (P $h 'supporting_evidence_ids'); if (-not $sup) { $sup = 'none' }
    $line += " $Dash evidence: " + $sup
    $ctr = Ids (P $h 'counter_evidence_ids')
    if ($ctr) { $line += " $Dash counter-evidence: " + $ctr }
    $o.Add($line)
    $reason = Md-Text (P $h 'reason') 500
    if ($reason) { $o.Add('   ' + $reason) }
  }
  $rejected = P $m 'rejected'
  $count = if ($rejected -is [array]) { $rejected.Count } else { 0 }
  $o.Add(''); $o.Add('Rejected by validation: ' + $count)
}
Emit ($o -join "`n")
if ($status -ceq 'done') { exit 0 }
exit 2
