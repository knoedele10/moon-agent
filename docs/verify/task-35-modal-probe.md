# task-35：官方前端探针适配「测试版警告」模态弹窗

改的文件只有 `docs/verify/frontend-probe.mjs`（verifier 自己的探针）；**没有动任何实现代码**，也没有 git commit。
实例一律 `APP_PORT=0`（本轮实测分配到 3281/5732/9294）+ `CUA_EXTRA_ARGS=--mock`。

## 0. 结论

| 项 | 结果 |
|---|---|
| 修好前（cua-ui 报的） | `F3c-dryrun-render` 红：`#cua-start` 被模态挡住点不到 |
| 修好后正常页面 | **PASS=21 FAIL=0 SKIP=0，exit 0**（含 F3c 恢复绿）|
| 负向对照 `--strip-dialog` | **PASS=18 FAIL=3，exit 1** —— 三条弹窗断言全红，其余照常绿 |

改的是「用户流程」（真人进来也得先关弹窗才能操作），**没有放松任何判定**；新增的弹窗断言已用变异证明抓得住回归（§3）。

## 1. 新增的弹窗断言（跑在 F3c 之前）

1. **`F3-modal-open`**：进入 `#/cua` 后断言 —— `dialog.open===true`、`role="dialog"`、`aria-modal="true"`、
   `aria-labelledby="cua-beta-title"`，并且用 **CDP AX 树**（`Accessibility.getFullAXTree`）找到 role=dialog 的节点，
   要求它的 **accessible name === 「测试版，别当真」**。实测证据：
   `{"open":true,"role":"dialog","ariaModal":"true","axName":"测试版，别当真","axModal":true}`
2. **`F3-modal-close`**：点「知道了，我胆子大」→ 断言 `open===false` **且焦点回到「电脑操控」页签**。
   实测证据：`{"open":false,"focusView":"cua","focusId":null}`
3. **`F3-modal-other-tabs:chat|models|keys|kb`**：边界 —— 其他 4 个页签各切一次，`dialog.open` 恒为 `false`（4/4 PASS）。
4. **`F3-modal-reenter`**：再切回「电脑操控」会**重新弹出**（需求与 cua.js 注释都写「每次进入即弹」，不是只弹一次）。

## 2. F3b 选择器（你问值不值得改 —— 值得，已改）

原来数 `#view-cua button`，而 task-22 的历史行是动态生成的 `<button class="cua-history-row">`，条数一变数字就飘（10/11/12），
**数字飘会掩盖真实变化**（真丢了一个控件也看不出来）。现在改成断言一组固定关键控件都在：
`#cua-task #cua-model #cua-dry-run #cua-start #cua-stop`，缺哪个直接报哪个 ✓

## 3. 负向对照：弹窗没出现时，断言必须转红

新增 `--strip-dialog` 模式：**只在 Playwright 的响应拦截里**把返回 HTML 中的 `<dialog id="cua-beta-dialog">` 删掉
（仓库文件一行没动），其余流程完全照跑。结果：

```
[FAIL] F3-modal-open   模态测试版警告没按契约弹出
[FAIL] F3-modal-close  关不掉模态（背景被 inert 时这正是操作被挡住的原因）
[FAIL] F3-modal-reenter 再次进入没有重新弹出（或关不掉）
[PASS] F3-modal-other-tabs:chat|models|keys|kb   （弹窗压根没出现过，所以恒为 false —— 符合预期）
[PASS] F3c-dryrun-render                        （没有模态挡着，点击自然成功）
---- 汇总：PASS=18 FAIL=3 ----  exit=1
```

两点价值：(a) 弹窗断言**确实抓得住**「弹窗没出现」这个回归；(b) 同一份对照里 `F3c` 反而变成 PASS，
反过来证明修好前 F3c 变红的**唯一原因就是模态把背景置为 inert**。

## 4. 我自己踩的一个假 FAIL（如实记录）

第一版 `F3-modal-close` 是**红的**：证据 `{"open":false,"focusView":null,"focusId":"cua-beta-ok"}` ——
关闭后焦点还在关闭按钮上。查实现：`cua.js:989 addEventListener("close", onBetaClosed)`，而 `close` 事件是**异步派发**的，
我的断言在 `open===false` 后立刻读 `activeElement`，**抢在事件处理之前**了 —— 是**我的竞态**，不是产品缺陷。
改法：给焦点一个 3s 落定窗口再断言（`waitForFunction` + 超时容忍），真缺陷是「永远回不去」，那时照样 FAIL ✓
改完实测 `{"open":false,"focusView":"cua"}` 全绿。

## 5. 未覆盖面

1. **ESC 关闭路径没断言**：契约与实现都说原生 `<dialog>` 自带 ESC 关闭，我只验了「点按钮」这条路径。
2. **稳定性只跑了单次**：正常/对照各跑一次；没有做 N 次重复统计抖动。
3. AX 树只查了 `role=dialog` 那个节点（名字、modal 属性），没有校验整棵树的顺序/背景是否被标记 inert。
4. 真实鼠标键盘的 CUA 运行仍然一律没做（安全红线）；本轮 F3c 走的仍是 dry-run + `--mock`。

## 6. 复跑

```powershell
# 起实例（注意 READY 在 stderr；不要和别人的实例抢端口）
$env:APP_PORT="0"; $env:CUA_EXTRA_ARGS="--mock"
#   <exe> -> 从 stderr 的 READY 行拿 http://127.0.0.1:<port>

node docs/verify/frontend-probe.mjs --base http://127.0.0.1:<port> --start                 # 期望 21/0/0
node docs/verify/frontend-probe.mjs --base http://127.0.0.1:<port> --strip-dialog --start  # 期望 3 条弹窗断言红
```

证据日志：docs/verify/tmp/fe35-normal.log、docs/verify/tmp/fe35-strip.log。
