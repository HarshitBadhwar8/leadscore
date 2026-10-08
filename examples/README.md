# Examples

| You want | Example |
|---|---|
| To score a CSV of leads and get ranked lists, as the Quick start does | [score-a-csv](score-a-csv/) |
| To see why a lead scored as it did | [explain-a-lead](explain-a-lead/) |
| To preview a run before it writes anything, then run it with pushes off | [dry-run-first](dry-run-first/) |
| To change a rule in the rubric and see the ranking change | [change-the-rubric](change-the-rubric/) |

Run them all from the repository root with `scripts/run-examples.sh`. It checks each script
with shellcheck, runs it, and compares its output with the last block in that example's
README.

Each script builds leadscore and scores the made-up leads in `examples/leads.csv` in a
temporary folder, with an empty home directory and no network. None of them needs an account
or a key. The output shown in each README writes the temporary folder as `/home/you/leadscore`,
each run id as `<run id>` and each lead id as `<lead id>`.
