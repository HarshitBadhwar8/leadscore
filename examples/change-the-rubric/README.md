The rubric is a YAML file that leadscore reads fresh on every run, so changing who ranks first
is an edit and a rerun. This one scores the made-up leads, then makes a warm path in worth 30
points instead of 10, checks the edited rubric with `leadscore rules check`, and runs again.
Jonas Brandt, the one top lead with a warm path, moves from second to first.

```sh
examples/change-the-rubric/run.sh
```

```text
full_name,score
Anna Weber,65
Jonas Brandt,60
Lea de Vries,50
Pia Schulz,50
rubric.yml: ok (version r-91146970c8582a94, 2 lanes, 0 detectors)
full_name,score
Jonas Brandt,80
Anna Weber,65
Lea de Vries,50
Pia Schulz,50
```
