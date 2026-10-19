# task-31：复验 task-23 的 FAIL（task-30 修复）

复验人：verifier（独立于 cua-go）  时间：2026-10-11 20:2x~20:3x（Asia/Shanghai）
复验对象：`electron/ready.js` + `electron/ready.test.js`（task-30），被验缺陷是我在 task-28 报的那条 FAIL。

## 0. 结论

| 复验点 | 判定 | 依据 |
|---|---|---|
| 1) 两个方向（只在 stdout / 只在 stderr）都能解析 | **PASS** | 我的探针 R1/R2；且真实后端实测 READY 只在 **stderr**，仍被解析出来 |
| 2) 分块边界：一行被切开不得解析出残缺 URL | **PASS** | 穷举 47 个切分点：既不早 settle、结果都是完整 URL；突变体（回到按 chunk 匹配）被我的 R3 抓到 |
| 3) 他们的 5 条能否区分「修好/没修好」 | **PASS（有保留）** | 两个关键突变都被抓到；但「不接 stderr」这条会让套件**挂起**而不是失败（无超时） |
| 4) 打包布局壳侧解析并 loadURL | **PASS** | 真实 `dist/win-unpacked/blog-my.exe` 起来后：壳日志给出 `http://127.0.0.1:1606`、该地址 `/api/health`=200；另有「真实后端两条流 → 真实 ready.js」的 85ms 解析 |

**原 FAIL 判定撤销**：task-30 的修复在**黑盒层面**成立。安装包层面的未覆盖面见 §4。

## 1. 复原缺陷 → 修复的对照（我自己造的两个突变）

我把 `ready.js` 按名字变异（写进隔离临时目录，**不动仓库文件**），再分别跑「他们的 5 条」和「我的探针」：

| 变异 | 他们的 5 条 | 我的探针（9 条判据） |
|---|---|---|
| clean（原样） | exit=0，pass 5 / fail 0 | PASS=8 FAIL=0 WARN=1，exit=0 |
| `stdout_only`（还原原缺陷：不接 stderr） | **HANG（>30s 不结束）** | **FAIL=3**（R2 stderr 方向、R3 分块穷举、R5 CRLF），exit=1 |
| `no_line_split`（回到「按 chunk 直接匹配」，半行会命中） | exit≠0，pass 4 / **fail 1**（`✖ 一行被管道切成两个 chunk 也能解析`） | **FAIL=1**（R3），exit=1 |
| `no_cap`（去掉 4KB 上限） | pass 5（**没抓到**） | FAIL=0（**也没抓到**） |

- **两个关键突变都被挡住**：方向错配、以及我上一轮点出的「分块边界同源变体」，两边都能区分 ✓
- **保留意见（建议改）**：`stdout_only` 突变下他们的套件是**挂起**而不是失败——因为用例直接 `await` 一个永远不会 settle 的 promise，没有超时。
  在 CI 里这表现为「卡住/超时」而不是「红」。建议给用例加超时（例如 `Promise.race` + 断言超时即失败），让回归**快速变红**。
- `no_cap` 两边都没覆盖：4KB 上限只是内存保护、不是协议语义，**不作为验收缺口**，仅记录。
- 方法学：第一轮我把两批突变跑在**同一个临时目录**、且并发执行，互相覆盖文件 → 结果互相矛盾，**已作废**；
  上表是隔离目录（`%TEMP%\ready-mut-clean1`）、串行、各带 30s 上限的干净结果。

## 2. 我的独立探针（`docs/verify/tmp/ready-probe.mjs`，直接 require 被验模块）

9 条判据、跑在 4 个变异体上；对原样代码的结果：**PASS=8 FAIL=0 WARN=1**

- R1 只在 stdout / R2 只在 stderr → 都能解析
- R3 **穷举 47 个切分点**：换行到达前不得 settle；最终结果必须是完整 URL（这是原缺陷的同源变体，也是本轮重点）
- R4（WARN）整行本身残缺（`READY http://127.0.0.1:` 就换行）仍会被当成 URL —— 见 §3 的残余风险说明
- R5 CRLF 行尾 / R6 噪声·小写·非 URL 不误报 / R7 先出现的胜出 / R8 5000 字符无换行噪声后仍能解析 / R9 超长行

## 3. 残余风险（如实记录，不阻断验收）

1. `waitForReady` 只对**完整行**匹配，但如果**后端自己**写出一行残缺 URL 并换行，仍会被解析成 `http://127.0.0.1:`。
   实测证据表明不会发生：真实后端的 READY 是整行 `2026/10/11 20:32:06 READY http://127.0.0.1:1600` 一次写出（§4 A）。
2. 导出的 `parseReadyUrl()` 本身不要求换行 —— 直接拿它去解析任意 chunk 仍可能得到残缺 URL。
   走 `waitForReady` 的路径是安全的；**建议**在注释里点明「不要用 parseReadyUrl 解析不完整块」。

## 4. 打包布局：壳侧解析并 loadURL（本轮的硬证据）

### A) 真实后端两条流 → 真实 ready.js（不需要 Electron GUI）
```
# 后端 = E:\blog-my\dist\win-unpacked\resources\bin\blog-my.exe  cwd = ...\resources  .env 变量数 = 18
[PASS] P1-shell-resolves  ready.js 从真实后端的真实两条流里解析出 http://127.0.0.1:1600（85ms）
[INFO] P2-stream  READY 出现在 stderr=true / stdout=false；stdout 字节=6884 stderr 字节=562
[INFO] P2-line    2026/10/11 20:32:06 READY http://127.0.0.1:1600
[PASS] P3-health  解析出的 URL 上 /api/health = 200
```
关键点：READY **只在 stderr**（stdout 那 6884 字节全是 GIN 噪声），壳仍解析成功 —— 这正是原缺陷的修复面。

### B) 真实打包 Electron 应用（`dist/win-unpacked/blog-my.exe`）
```
存活20s=True，后端进程数=5，stdout 4251 字节
[electron] 配置来源: C:\Users\woshiss\AppData\Roaming\blog-my-desktop\.env
[electron] loading http://127.0.0.1:1606
[electron] loadURL resolved
[electron] window shown, bounds = { x: 213, y: 133, width: 1282, height: 802 }
--- 进程/端口事实 ---
# 后端 = E:\blog-my\dist\win-unpacked\resources\bin\blog-my.exe  cwd = E:\blog-my\dist\win-unpacked\resources  .env 变量数 = 18
[PASS] P1-shell-resolves  ready.js 从真实后端的真实两条流里解析出 http://127.0.0.1:1600（85ms）
[INFO] P2-stream  READY 出现在 stderr=true / stdout=false；stdout 字节=6884 stderr 字节=562
[INFO] P2-line    2026/10/11 20:32:06 READY http://127.0.0.1:1600
[PASS] P3-health  解析出的 URL 上 /api/health = 200（body 前 50 字：{"service":"blog-my","status":"ok","time":"2026-10）
---- 汇总：FAIL=0 ----
```
- 壳日志里的 `http://127.0.0.1:1606`，我直接请求它的 `/api/health` → **200**：说明壳不只是「看见了 READY」，而是**真的拿到了可用地址并指向了后端**。
- `[electron] 配置来源: C:\Users\woshiss\AppData\Roaming\blog-my-desktop\.env`：安装态走 userData 的 .env（task-23 的打包逻辑）✓

**复现注意（环境坑，不是产品问题）**：本会话（DSH 本身是 Electron 应用）里 `ELECTRON_RUN_AS_NODE=1` 会被子进程继承，
直接启动打包 exe 会**当成 node 跑**（表现为：进程瞬间退出、stdout/stderr 全空；带参数时报 `bad option: --enable-logging`）。
必须先 `$env:ELECTRON_RUN_AS_NODE=$null` 再启动。我自己先踩了这个坑（前两次打包态实测全空），记在这里免得别人重踩。

## 5. 未覆盖面

1. **安装包未重出**：`dist\blog-my Setup 0.1.0.exe` 时间戳 **19:30:44**，而 `electron/ready.js` 是 **19:48:16**、`package.json` 19:49:05 —— 安装包**早于**本次修复（cua-go 报 electron-builder 连不上 GitHub `ETIMEDOUT 20.205.243.166:443`）。
   我验的是 `dist\win-unpacked\resources\app.asar`（19:56:30，手工重打）：用字节搜索能命中本次新增的两处标记（注释「两条流都接进同一个探测器」与 `pending.length > 4096`）✓；
   同一个搜索在 Setup.exe 里**没有命中**（NSIS 有压缩，不能当反证，但结合时间戳可以确认它不含本次修复）。
   → **「装一次 Setup.exe 再双击」这条没有人验过。**
2. `loadURL resolved` / 窗口尺寸那两行是 cua-go 的日志；我这边独立拿到的是「壳解析出 URL + 该 URL 活着」。
3. 我自己的探针 R4/R9 是 WARN/INFO，不改判定；`no_cap` 无人覆盖（见 §1）。
4. 真实鼠标键盘的 CUA 运行仍然一律没做（安全红线），与本任务无关。

## 6. 复跑命令

```powershell
# ① 他们的 5 条
npm run test:electron

# ② 我的独立探针（可直接指定被验模块路径）
node docs/verify/tmp/ready-probe.mjs electron/ready.js

# ③ 真后端两条流 → 真 ready.js（打包产物，不需要 GUI）
node docs/verify/tmp/pkg-ready-e2e.mjs .env

# ④ 真打包应用（必须先清 ELECTRON_RUN_AS_NODE）
$env:ELECTRON_RUN_AS_NODE=$null; $env:ELECTRON_ENABLE_LOGGING="1"
Start-Process "dist\win-unpacked\blog-my.exe" -PassThru

# ⑤ 突变对照（隔离目录，别与别人共用一个 tmp 目录）
node docs/verify/tmp/ready-mutate.mjs no_line_split "$env:TEMP\my-mut"
node --test "$env:TEMP\my-mut\ready.test.js"
```

## 7. 声明

- 只写 `docs/verify/**`（探针与报告），**没有修改实现代码，也没有 git commit**。
- 突变体一律写在隔离临时目录，仓库里的 `electron/ready.js` 全程未被改动。
- 我自己踩的两个坑（并发写同一突变目录 → 结果作废重跑；`ELECTRON_RUN_AS_NODE` 导致打包 exe 变 node）都如实写在上面。
