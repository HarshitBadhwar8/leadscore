# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
# Renders .github/assets/demo.gif: the README's quick start, typed verbatim,
# then one explain. Run from the repo root with `vhs .github/assets/demo.tape`
# (.github/workflows/demo.yml does). It needs vhs and Go. The hidden setup
# builds leadscore and does the Quick start's folder steps in an empty temp
# dir, which is also HOME. It scores only the made-up leads in
# examples/leads.csv, and nothing it runs uses the network.
Output .github/assets/demo.gif
Set Shell bash
Set Width 1200
Set Height 520
Set Padding 30
Set FontSize 14
Set TypingSpeed 40ms
Set WaitPattern /\$\s*$/
Env PS1 "$ "
Hide
Type 'src=$PWD && cd "$(mktemp -d)" && (cd "$src" && go build -o "$OLDPWD/leadscore" ./cmd/leadscore) && export HOME=$PWD' Enter
Wait@5m
Type `mkdir try && cp $src/examples/leads.csv try/leads.csv && cp $src/examples/rubric.csv-only.yml try/rubric.yml && cd try` Enter
Type `printf 'version: 1\nstore: { type: sqlite, path: leadscore.db }\nsources:\n  - { id: leads, type: csv, path: leads.csv }\nexport: { dir: out }\n' > leadscore.yml` Enter
Type "clear" Enter
Show
Type "../leadscore run" Sleep 500ms Enter
Wait
Sleep 1s
Type "../leadscore ranked --csv | cut -d, -f4,10,12" Sleep 500ms Enter
Wait
Sleep 1s
Type "ls out" Sleep 500ms Enter
Wait
Sleep 3s
Hide
Type "clear" Enter
Show
Type "../leadscore explain anna.weber@kranlogistik.example" Sleep 500ms Enter
Wait
Sleep 5s
