@echo off
rem 假 CUA_PYTHON：制造"子进程 + 孙进程"两层，用来独立复验 stop 是否真的收掉整棵进程树。
start /b "" python "E:\blog-my\docs\verify\tmp\gc.py" "%TEMP%\cua-grandchild.pid"
python "E:\blog-my\docs\verify\tmp\gc.py" "%TEMP%\cua-direct-child.pid"
