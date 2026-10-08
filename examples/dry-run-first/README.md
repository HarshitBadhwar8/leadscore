`leadscore run --dry-run` prints one line per lead whose verdict, status or lane would change,
then totals. It takes no lease, writes nothing and spends nothing, so `ls` after it shows no
store and no lists. This one previews a first run, does the real run with
`pushes_enabled: false`, then previews again to show nothing would change. Do this before
every rubric change and before you turn pushes on.

```sh
examples/dry-run-first/run.sh
```

```text
dry run: no store yet at /home/you/leadscore/try/leadscore.db; scoring as on a first run
dry run: no lease taken, nothing written, no enrichment, no lookups and no pushes
new      <lead id>  fit_signal=yes tier=1 score=65 status=new lane=call-list
new      <lead id>  fit_signal=yes tier=1 score=60 status=new lane=call-list
new      <lead id>  fit_signal=yes tier=2 score=50 status=new lane=call-list
new      <lead id>  fit_signal=yes tier=1 score=50 status=new lane=call-list
new      <lead id>  fit_signal=yes tier=1 score=40 status=new lane=call-list
new      <lead id>  fit_signal=none tier=none score=15 status=new lane=nurture
new      <lead id>  fit_signal=yes tier=4 score=0 status=new lane=none
new      <lead id>  fit_signal=yes tier=4 score=0 status=new lane=none
totals: 8 lead(s) scored: 8 new, 0 changed, 0 unchanged; planned lanes: call-list 5, none 2, nurture 1
dry run (healthy): 8 lead(s) would be scored from 8 input row(s), 0 left for later runs; nothing was saved or pushed
leads.csv
leadscore.yml
rubric.yml
run <run id>: healthy; 8 lead(s) scored, 8 input row(s) merged, 0 left for later runs, 0 pushed
call-list.csv
nurture.csv
dry run: no lease taken, nothing written, no enrichment, no lookups and no pushes
totals: 8 lead(s) scored: 0 new, 0 changed, 8 unchanged; planned lanes: call-list 5, none 2, nurture 1
dry run (healthy): 8 lead(s) would be scored from 0 input row(s), 0 left for later runs; nothing was saved or pushed
```
