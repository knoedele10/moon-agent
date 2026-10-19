# Day 21 同伴交叉验证脚本（可重复执行，作为「可打开 / 可读写 / 无报错」三行结论的证据）
# 用法：powershell -ExecutionPolicy Bypass -File scripts/verify-day21.ps1
# 说明：只做读与一次写入复验，不改任何业务代码；写入的收藏标题带时间戳，重复执行不会污染判定。
param(
  [string]$Base = 'https://blog-my-check-10974.app.workbuddy.host',
  [string]$Origin = 'http://localhost:8091',
  [string]$LogPath = ''
)
$ErrorActionPreference = 'Continue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$script:log = New-Object System.Collections.ArrayList
function Say($s) { [void]$script:log.Add([string]$s) }

$stamp = Get-Date -Format 'yyyy-MM-dd HH:mm:ss'
$uniqueTitle = 'Day 21 交叉验证：公网真实读写往返 ' + (Get-Date -Format 'HHmmss')
$script:pass = 0
$script:fail = 0

function Req {
  param([string]$Name, [string]$Method, [string]$Path, [string]$Body, [string]$ContentType, [string]$Expect)
  $url = $Base + $Path
  $headers = @{}
  if ($Origin) { $headers['Origin'] = $Origin }
  $p = @{ Uri = $url; Method = $Method; UseBasicParsing = $true; Headers = $headers; TimeoutSec = 40 }
  if ($Body -ne $null -and $Body -ne '') {
    # 关键：PowerShell 5.1 的 Invoke-WebRequest 把字符串请求体按 ASCII 发送，中文会变成 '?'。
    # 这里显式转成 UTF-8 字节再发，服务端才收得到正确的中文。
    $p['Body'] = [System.Text.Encoding]::UTF8.GetBytes($Body)
    if ($ContentType) { $p['ContentType'] = $ContentType }
  }
  $code = 0; $text = ''; $cors = ''; $ms = 0
  $sw = [Diagnostics.Stopwatch]::StartNew()
  try {
    $resp = Invoke-WebRequest @p
    $sw.Stop(); $ms = $sw.ElapsedMilliseconds
    $code = [int]$resp.StatusCode
    $text = [string]$resp.Content
    if ($resp.Headers['Access-Control-Allow-Origin']) { $cors = [string]$resp.Headers['Access-Control-Allow-Origin'] }
  } catch {
    $sw.Stop(); $ms = $sw.ElapsedMilliseconds
    $r = $_.Exception.Response
    if ($r -ne $null) {
      $code = [int]$r.StatusCode
      try { $sr = New-Object System.IO.StreamReader($r.GetResponseStream()); $text = $sr.ReadToEnd() } catch { $text = '' }
      try { $cors = [string]$r.Headers['Access-Control-Allow-Origin'] } catch { }
    } else {
      $code = 0; $text = $_.Exception.Message
    }
  }
  $flat = ($text -replace '\s+', ' ')
  if ($flat.Length -gt 140) { $flat = $flat.Substring(0, 140) + '...' }
  $ok = $false
  if ($Expect -eq '2xx') { $ok = ($code -ge 200 -and $code -lt 300) }
  elseif ($Expect -eq '204') { $ok = ($code -eq 204) }
  elseif ($Expect.StartsWith('body:')) { $ok = (($code -ge 200 -and $code -lt 300) -and ($text -match [regex]::Escape($Expect.Substring(5)))) }
  else { $ok = ($code.ToString() -eq $Expect) }
  if ($ok) { $script:pass++ } else { $script:fail++ }
  $tag = 'FAIL'; if ($ok) { $tag = 'PASS' }
  $corsNote = ''; if ($cors) { $corsNote = ' CORS=' + $cors }
  Say ("[{0}] {1,-7} {2,-32} -> {3} ({4}ms){5} | {6}" -f $tag, $Method, $Path, $code, $ms, $corsNote, $flat)
  return $text
}

Say ('== Day 21 交叉验证 @ ' + $stamp + ' ==')
Say ('Base = ' + $Base)
Say ''
Say '-- 第 1 组：可打开（页面 + 静态资源 + 探活）--'
Req 'health'     'GET' '/api/health'           $null $null 'body:status":"ok'
Req 'index page' 'GET' '/'                     $null $null 'body:blog-my 检查台'
Req 'hot page'   'GET' '/hot'                  $null $null 'body:今日热搜'
Req 'chat page'  'GET' '/chat'                 $null $null '2xx'
Req 'css asset'  'GET' '/static/css/style.css' $null $null '2xx'

Say ''
Say '-- 第 2 组：可读写（真实数据读 + 写入 + 回读 + 边界校验）--'
$hot = Req 'hot read'            'GET'  '/api/hot?limit=3'  $null $null 'body:fetched_at'
$before = Req 'favorites read'   'GET'  '/api/favorites'    $null $null 'body:ok":true'
$body = '{"title":"' + $uniqueTitle + '","url":"' + $Base + '/"}'
Req 'favorites write'         'POST' '/api/favorites' $body 'application/json' '201'
Req 'duplicate rejected'      'POST' '/api/favorites' $body 'application/json' '409'
Req 'missing title rejected'  'POST' '/api/favorites' '{"url":"https://example.com"}' 'application/json' '400'
Req 'bad json rejected'       'POST' '/api/favorites' '{not json' 'application/json' '400'
$after = Req 'favorites reread'  'GET'  '/api/favorites'    $null $null '2xx'
$persisted = ($after -match [regex]::Escape($uniqueTitle))
if ($persisted) { $script:pass++; Say '[PASS] persistence      GET     /api/favorites (回读新写入项) -> 命中本次写入标题' }
else { $script:fail++; Say '[FAIL] persistence      GET     /api/favorites (回读新写入项) -> 未命中本次写入标题' }
$beforeN = ([regex]::Matches($before, '"title"')).Count
$afterN = ([regex]::Matches($after, '"title"')).Count
$hotN = ([regex]::Matches($hot, '"rank"')).Count
Say ("[INFO] 收藏条数：写入前 {0} -> 写入后 {1}" -f $beforeN, $afterN)
Say ("[INFO] /api/hot?limit=3 返回条目数：" + $hotN)
Say ("[INFO] 本次写入标题：" + $uniqueTitle)

Say ''
Say '-- 第 3 组：无报错（跨域预检 + 错误分支状态码）--'
Req 'cors preflight' 'OPTIONS' '/api/favorites' $null $null '204'
Req 'cors header'    'GET'     '/api/health'    $null $null 'body:status":"ok'

Say ''
Say ('== 汇总：PASS ' + $script:pass + ' / FAIL ' + $script:fail + ' ==')
Write-Output ($script:log -join [Environment]::NewLine)
if ($LogPath -ne '') {
  [System.IO.File]::WriteAllLines($LogPath, $script:log.ToArray(), (New-Object System.Text.UTF8Encoding($false)))
  Write-Output ('log written: ' + $LogPath)
}
if ($script:fail -gt 0) { exit 1 }
