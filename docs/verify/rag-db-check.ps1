# Independent RAG database-level cascade check (verifier-authored; SQL evidence, no implementer scripts).
# Usage: powershell -NoProfile -File docs/verify/rag-db-check.ps1 [-Base http://127.0.0.1:8300]
# All output labels are ASCII so Windows PowerShell 5.1 cannot mojibake them.
param([string]$Base = "http://127.0.0.1:8300")

$ErrorActionPreference = "Continue"
$env:MYSQL_PWD = (Get-Content .env | Where-Object { $_ -match '^DB_PASSWORD=' }) -replace '^DB_PASSWORD=', ''
$tmp = "docs/verify/tmp"
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
$ck = (Resolve-Path $tmp).Path + "\cookies.txt"
$payload = (Resolve-Path $tmp).Path + "\payload.json"

function Q([string]$sql) {
  Write-Output ("SQL> " + $sql)
  mysql -h 127.0.0.1 -P 3306 -u root blog_my --batch --raw -e $sql 2>&1 | Where-Object { $_ -notmatch 'Using a password' }
  Write-Output ""
}
function SaveJson($obj) {
  $json = $obj | ConvertTo-Json -Compress -Depth 5
  [IO.File]::WriteAllText($payload, $json, (New-Object Text.UTF8Encoding($false)))
}
function PostJson([string]$path, $obj) {
  SaveJson $obj
  return (curl.exe -s -b $ck -H "Content-Type: application/json; charset=utf-8" --data-binary "@$payload" ($Base + $path)) -join ""
}

$u = "vrf_db_" + (Get-Date -Format "HHmmss")
Write-Output "== 1) register + login (curl.exe, cookie jar) =="
$reg = (curl.exe -s -o NUL -w "%{http_code}" -X POST --data "username=$u&password=Verifier123" ($Base + "/register")) -join ""
$log = (curl.exe -s -o NUL -w "%{http_code}" -c $ck -X POST --data "username=$u&password=Verifier123" ($Base + "/login")) -join ""
Write-Output ("register HTTP " + $reg + " ; login HTTP " + $log)
if (-not (Test-Path $ck)) { Write-Output "no cookie jar written -> abort"; exit 2 }
Write-Output ("cookie names: " + ((Get-Content $ck | Where-Object { $_ -match "`t" } | ForEach-Object { ($_ -split "`t")[5] }) -join " | "))
Write-Output ""

Write-Output "== 2) ingest a long text (should produce multiple chunks) =="
$paras = @()
for ($i = 1; $i -le 24; $i++) {
  $paras += "Paragraph $i : vector retrieval and chunking demo text for local knowledge base, marker VRFDB-$i, used to check kb_chunk row count and embedding byte length."
}
$text = ($paras -join "`n`n")
$t0 = Get-Date
$resp = PostJson "/api/kb/text" @{ title = "VRF DB cascade $u"; text = $text }
$ms = [int]((Get-Date) - $t0).TotalMilliseconds
Write-Output ("ingest response: " + $resp + "  (client-observed " + $ms + "ms)")
$r = $resp | ConvertFrom-Json
$id = $r.id
Write-Output ("document id = " + $id + " ; API-reported chunk_count = " + $r.chunk_count)
Write-Output ""

Write-Output "== 3) BEFORE delete: direct SQL =="
Q "SELECT id, kind, bytes, chunk_count, (sha256 IS NOT NULL) AS has_sha FROM kb_document WHERE id=$id;"
Q "SELECT COUNT(*) AS rows_in_kb_chunk, MIN(dim) AS min_dim, MAX(dim) AS max_dim, MIN(LENGTH(embedding)) AS min_emb_bytes, MAX(LENGTH(embedding)) AS max_emb_bytes FROM kb_chunk WHERE document_id=$id;"
Q "SELECT seq, kind, dim, LENGTH(embedding) AS emb_bytes, CHAR_LENGTH(text) AS text_chars FROM kb_chunk WHERE document_id=$id ORDER BY seq;"

Write-Output "== 4) search hits the document (before delete) =="
$s1 = PostJson "/api/kb/search" @{ query = "vector retrieval and chunking demo VRFDB-7"; top_k = 3 } | ConvertFrom-Json
Write-Output ("hits = " + (($s1.hits | ForEach-Object { "$($_.document_id)/seq$($_.seq)/$([math]::Round($_.score,4))" }) -join ", "))
Write-Output ""

Write-Output "== 5) DELETE the document =="
$del = (curl.exe -s -b $ck -X DELETE ($Base + "/api/kb/documents/" + $id)) -join ""
Write-Output ("delete body: " + $del)
Write-Output ""

Write-Output "== 6) AFTER delete: direct SQL (chunks must be gone too) =="
Q "SELECT COUNT(*) AS document_rows_left FROM kb_document WHERE id=$id;"
Q "SELECT COUNT(*) AS chunk_rows_left FROM kb_chunk WHERE document_id=$id;"
Q "SELECT COUNT(*) AS orphan_chunks_global FROM kb_chunk c LEFT JOIN kb_document d ON d.id=c.document_id WHERE d.id IS NULL;"
Q "SELECT COUNT(*) AS bad_dim_global FROM kb_chunk WHERE dim<>1024;"
Q "SELECT COUNT(*) AS bad_emb_bytes_global FROM kb_chunk WHERE LENGTH(embedding)<>4096;"
Q "SELECT COUNT(*) AS chunk_count_mismatch_global FROM (SELECT d.id FROM kb_document d LEFT JOIN kb_chunk c ON c.document_id=d.id GROUP BY d.id, d.chunk_count HAVING d.chunk_count<>COUNT(c.id)) t;"
Q "SELECT COUNT(*) AS dup_sha256_global FROM (SELECT sha256 FROM kb_document WHERE sha256 IS NOT NULL GROUP BY sha256 HAVING COUNT(*)>1) t;"

Write-Output "== 7) search must not hit it anymore =="
$s2 = PostJson "/api/kb/search" @{ query = "vector retrieval and chunking demo VRFDB-7"; top_k = 3 } | ConvertFrom-Json
$still = @($s2.hits | Where-Object { $_.document_id -eq $id })
Write-Output ("hits pointing at the deleted document = " + $still.Count)
Write-Output ""

Write-Output "== 8) cross-lingual semantic check (Chinese query -> English document) =="
$en = "Roses should be pruned in late winter while the plant is still dormant. Cut dead or crossing canes first, then shorten the remaining stems by one third. Always make the cut just above an outward facing bud so new growth opens up the centre of the bush."
$r8 = PostJson "/api/kb/text" @{ title = "VRF rose pruning $u"; text = $en } | ConvertFrom-Json
$enId = $r8.id
Write-Output ("English document id = " + $enId + " chunk_count = " + $r8.chunk_count)
$cn = PostJson "/api/kb/search" @{ query = "玫瑰花应该在什么季节修剪，剪在哪里比较好？"; top_k = 3 } | ConvertFrom-Json
Write-Output ("hits = " + (($cn.hits | ForEach-Object { "$($_.document_id)/seq$($_.seq)/$([math]::Round($_.score,4))/" + $_.text.Substring(0, [Math]::Min(40, $_.text.Length)) }) -join " || "))
if ($cn.hits.Count -gt 0 -and $cn.hits[0].document_id -eq $enId) {
  Write-Output ("SEMANTIC_TOP1_OK score=" + [math]::Round($cn.hits[0].score,4))
} else {
  Write-Output "SEMANTIC_TOP1_MISS"
}
$d8 = (curl.exe -s -b $ck -X DELETE ($Base + "/api/kb/documents/" + $enId)) -join ""
Write-Output ("cleanup English doc: " + $d8)
Write-Output ""

Write-Output "== 9) image upload: which vision model was used (kb_document.meta) =="
$png = (Resolve-Path "docs/verify/tmp/zebra-verify.png").Path
$t1 = Get-Date
$up = (curl.exe -s -b $ck -F ("file=@" + $png) ($Base + "/api/kb/documents")) -join ""
$ms1 = [int]((Get-Date) - $t1).TotalMilliseconds
Write-Output ("upload response: " + $up + "  (client-observed " + $ms1 + "ms)")
$img = $up | ConvertFrom-Json
$imgId = $img.id
Q "SELECT id, kind, title, bytes, chunk_count, meta FROM kb_document WHERE id=$imgId;"
Q "SELECT seq, kind, dim, LENGTH(embedding) AS emb_bytes, LEFT(text, 120) AS caption_head FROM kb_chunk WHERE document_id=$imgId ORDER BY seq;"
$s9 = PostJson "/api/kb/search" @{ query = "ZEBRA-7731"; top_k = 3 } | ConvertFrom-Json
Write-Output ("image retrieval hits = " + (($s9.hits | ForEach-Object { "$($_.document_id)/$($_.kind)/$([math]::Round($_.score,4))" }) -join ", "))
$d9 = (curl.exe -s -b $ck -X DELETE ($Base + "/api/kb/documents/" + $imgId)) -join ""
Write-Output ("cleanup image doc: " + $d9)
Q "SELECT COUNT(*) AS chunk_rows_left_for_image FROM kb_chunk WHERE document_id=$imgId;"
Write-Output ""

Write-Output "== 10) cleanup: drop verifier test users, confirm empty =="
Q "DELETE FROM user WHERE username LIKE 'vrf\_%';"
Q "SELECT COUNT(*) AS vrf_users_left FROM user WHERE username LIKE 'vrf\_%';"
Q "SELECT COUNT(*) AS documents_left FROM kb_document;"
Q "SELECT COUNT(*) AS chunks_left FROM kb_chunk;"
Remove-Item -Force $ck -ErrorAction SilentlyContinue
