import os, sys, time
open(sys.argv[1], "w").write(str(os.getpid()))
time.sleep(600)
