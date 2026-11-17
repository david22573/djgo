#!/data/data/com.termux/files/usr/bin/bash
# parity.sh — prove the Go CLI matches the bash `dj` for read-only verbs.
# Times drift between two runs, so normalize every m:ss/d.mmm to a placeholder.
GO="${GO:-$HOME/djgo/djgo}"
norm() { sed -E 's/[0-9]+:[0-9]{2}/T:TT/g; s/[0-9]+\.[0-9]+/N.N/g'; }

fail=0
for v in now status list; do
  a=$(dj "$v" 2>&1 | norm)
  b=$("$GO" "$v" 2>&1 | norm)
  if [ "$a" = "$b" ]; then
    echo "PASS $v"
  else
    echo "FAIL $v"
    diff <(printf '%s\n' "$a") <(printf '%s\n' "$b") | head -10
    fail=1
  fi
done
[ $fail -eq 0 ] && echo "parity: all read-only verbs identical"
exit $fail
