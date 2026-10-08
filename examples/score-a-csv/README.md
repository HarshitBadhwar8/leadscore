This is the README's quick start, after the clone and the build. It scores the made-up leads
in `examples/leads.csv` with the example rubric, prints the ranking, and lists the two export
lists the run wrote to `out/`. Start here to see what leadscore does before you point it at
your own leads.

```sh
examples/score-a-csv/run.sh
```

```text
run <run id>: healthy; 8 lead(s) scored, 8 input row(s) merged, 0 left for later runs, 0 pushed
full_name,score,lane
Anna Weber,65,call-list
Jonas Brandt,60,call-list
Lea de Vries,50,call-list
Pia Schulz,50,call-list
Ines Ruiz,40,call-list
Marie Laurent,15,nurture
Tom Hale,0,
Sam Ortiz,0,
call-list.csv
nurture.csv
```
