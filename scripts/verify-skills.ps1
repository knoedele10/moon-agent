# 技能管理 + 技能自动检索的端到端验证。
#
# 前置条件：服务已在 $Base 上跑起来（例如 $env:APP_PORT=8099; go run ./cmd/blog-my），
# 且 .env 里有可用的模型 Key（理解任务与重排都要真实调模型）。
#
# 验证五件事：
#   1. POST /api/skills/import 能把技能库导进来；
#   2. POST /api/skills/recall 会先“理解任务”改写查询，再做混合召回 + 大模型重排（source=llm）；
#   3. POST /api/chat 会先推 skills 事件（自动选中技能），模型能调 skill_search / skill_read；
#   4. 技能正文被自动注入后，模型确实照做；
#   5. 导入的 enabled 是三态：不传时不会把手工关掉的技能重新打开。
param(
  [string]$Base = "http://127.0.0.1:8099",
  [string]$SkillDir = "$env:USERPROFILE\.agents\skills",
  [string]$Model = "deepseek-chat"
)

$ErrorActionPreference = "Stop"
# 控制台按 UTF-8 输出：否则中文结果在 Windows PowerShell 5.1 下会显示成乱码。
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$OutputEncoding = [System.Text.Encoding]::UTF8

function Post-Json($session, $url, $obj) {
  $json = $obj | ConvertTo-Json -Depth 6 -Compress
  $bytes = [System.Text.Encoding]::UTF8.GetBytes($json)
  return Invoke-RestMethod -Uri $url -Method POST -WebSession $session -ContentType "application/json; charset=utf-8" -Body $bytes
}

# SSE 响应头是 text/event-stream（不带 charset），PowerShell 5.1 会按 Latin-1 解，
# 中文全成乱码。这里直接拿原始字节自己按 UTF-8 解。
function Read-Utf8Body($resp) {
  $ms = New-Object System.IO.MemoryStream
  $resp.RawContentStream.Position = 0
  $resp.RawContentStream.CopyTo($ms)
  return [System.Text.Encoding]::UTF8.GetString($ms.ToArray())
}

function Get-SkillsEvent($sse) {
  $m = [regex]::Match($sse, "event: skills\s*\r?\ndata: ([^\r\n]+)")
  if ($m.Success) { return $m.Groups[1].Value | ConvertFrom-Json }
  return $null
}

$user = "e2e_skill_" + (Get-Random)
$pass = "e2e-pass-123"
Write-Host "测试账号：$user"

$session = New-Object Microsoft.PowerShell.Commands.WebRequestSession
Invoke-WebRequest -Uri "$Base/register" -Method POST -WebSession $session -Body @{ username = $user; password = $pass } -UseBasicParsing | Out-Null
Invoke-WebRequest -Uri "$Base/login" -Method POST -WebSession $session -Body @{ username = $user; password = $pass } -UseBasicParsing | Out-Null

Write-Host ""
Write-Host "== 1. 导入技能库 =="
$imp = Post-Json $session "$Base/api/skills/import" @{ dir = $SkillDir }
Write-Host ("  root={0}" -f $imp.root)
Write-Host ("  扫描 {0} 个 SKILL.md，导入 {1} 条（新建 {2}，更新 {3}），同名副本 {4} 个，保留已有开关={5}" -f $imp.files, $imp.imported, $imp.created, $imp.updated, $imp.duplicates.Count, $imp.preserve_existing)
if ($imp.imported -lt 50) { throw "导入条数明显不对：$($imp.imported)" }

$list = Invoke-RestMethod -Uri "$Base/api/skills" -WebSession $session
$enabledCount = ($list.skills | Where-Object { $_.enabled }).Count
Write-Host ("  列表接口返回 {0} 条，其中已启用 {1} 条" -f $list.total, $enabledCount)

Write-Host ""
Write-Host "== 2. 理解任务 + 混合召回 + 大模型重排 =="
$queries = @(
  "把这段中文改得自然一点，别像机器写的",
  "帮我做一份 Word 文档，要有封面和目录",
  "解释一下这段报错",
  "帮我审一下这段代码"
)
foreach ($q in $queries) {
  $rec = Post-Json $session "$Base/api/skills/recall" @{ query = $q; model = $Model; top_k = 20; top_n = 5 }
  $picked = ($rec.picked | ForEach-Object { $_.name }) -join "、"
  Write-Host ("  查询：{0}" -f $q)
  Write-Host ("    检索串：{0}" -f $rec.recall_query)
  Write-Host ("    召回 {0} 条 → 选中 {1} 条（source={2}）：{3}" -f $rec.recall.Count, $rec.picked.Count, $rec.source, $picked)
  if ($rec.error) { Write-Host ("    降级说明：{0}" -f $rec.error) }
}

Write-Host ""
Write-Host "== 3. 对话：skills 事件 + skill_search / skill_read =="
$ask = "先用 skill_search 找有没有把中文润色得自然的技能，再用 skill_read 读它，然后按它的方法把这句话改自然：本产品致力于为用户提供优质的体验。"
$body = @{ model = $Model; content = $ask; tools = $true } | ConvertTo-Json -Compress
$resp = Invoke-WebRequest -Uri "$Base/api/chat" -Method POST -WebSession $session -ContentType "application/json; charset=utf-8" -Body ([System.Text.Encoding]::UTF8.GetBytes($body)) -UseBasicParsing -TimeoutSec 300
$sse = Read-Utf8Body $resp
$sk = Get-SkillsEvent $sse
$toolNames = [regex]::Matches($sse, '"name":"([a-z_]+)"') | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique
if ($sk) {
  Write-Host ("  skills 事件：source={0}，自动选中 {1} 条：{2}" -f $sk.source, $sk.picked.Count, (($sk.picked | ForEach-Object { $_.name }) -join "、"))
} else {
  Write-Host "  skills 事件：（没有）"
}
Write-Host ("  本轮出现的工具名：{0}" -f (($toolNames) -join "、"))
$hasSkillTool = ($toolNames -contains "skill_search") -or ($toolNames -contains "skill_read")
Write-Host ("  skill_search/skill_read 是否被真实调用：{0}" -f $(if ($hasSkillTool) { "是" } else { "否" }))

Write-Host ""
Write-Host "== 4. 技能正文被自动注入后，模型是否照做 =="
$body2 = @{ model = $Model; content = "用 caveman 风格（极度压缩、去掉废话）说一句：你好，世界。"; tools = $true } | ConvertTo-Json -Compress
$resp2 = Invoke-WebRequest -Uri "$Base/api/chat" -Method POST -WebSession $session -ContentType "application/json; charset=utf-8" -Body ([System.Text.Encoding]::UTF8.GetBytes($body2)) -UseBasicParsing -TimeoutSec 300
$sse2 = Read-Utf8Body $resp2
$sk2 = Get-SkillsEvent $sse2
if ($sk2) { Write-Host ("  自动选中的技能：{0}" -f (($sk2.picked | ForEach-Object { $_.name }) -join "、")) }
$reply = ([regex]::Matches($sse2, "event: delta\s*\r?\ndata: ([^\r\n]+)") | ForEach-Object {
  ($_.Groups[1].Value | ConvertFrom-Json).content
}) -join ""
Write-Host ("  回复：{0}" -f $reply)

Write-Host ""
Write-Host "== 5. 导入的 enabled 三态 =="
$target = $list.skills | Where-Object { $_.name -eq "caveman" } | Select-Object -First 1
$off = Invoke-RestMethod -Uri ("$Base/api/skills/" + $target.id) -Method PATCH -WebSession $session -ContentType "application/json" -Body ([System.Text.Encoding]::UTF8.GetBytes((@{ enabled = $false } | ConvertTo-Json -Compress)))
$again = Post-Json $session "$Base/api/skills/import" @{ dir = $SkillDir }
$after = Invoke-RestMethod -Uri "$Base/api/skills" -WebSession $session
$cav = $after.skills | Where-Object { $_.name -eq "caveman" } | Select-Object -First 1
Write-Host ("  手工关掉 caveman → 不传 enabled 重跑导入 → caveman enabled={0}（期望 False）" -f $cav.enabled)
$force = Post-Json $session "$Base/api/skills/import" @{ dir = $SkillDir; enabled = $true }
$after2 = Invoke-RestMethod -Uri "$Base/api/skills" -WebSession $session
$cav2 = $after2.skills | Where-Object { $_.name -eq "caveman" } | Select-Object -First 1
Write-Host ("  显式 enabled=true 重导 → caveman enabled={0}（期望 True）" -f $cav2.enabled)
if ($cav.enabled -ne $false -or $cav2.enabled -ne $true) { throw "三态语义不对" }

Write-Host ""
Write-Host "测试账号 $user 已保留在库里；清理时删掉该 user_id 的 agent_skill / conversation / message / user 行即可。"
