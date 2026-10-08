`leadscore explain` takes an email, a LinkedIn URL or a lead id and prints that lead's verdict
with every rule that set it. This one scores the made-up leads, then explains Anna Weber, who
tops the call list, and Tom Hale, who is on no list because his company is too small for the
rubric. Use it when a lead's rank surprises you, before you change the rubric.

```sh
examples/explain-a-lead/run.sh
```

```text
lead <lead id>
email: anna.weber@kranlogistik.example
linkedin: linkedin.com/in/fake-example-0001
name: Anna Weber
company: kranlogistik.example
status: new
lane: call-list
fit_signal: yes
tier: 1
score: 65 (account 50, contact 15)
reasons:
  - fit_signal = yes (rule 1: company.legacy_tool_seen = true)
  - tier = 1 (rule 3: fit_signal = yes and company.largest_fleet >= 20)
  - +40 account: tier = 1
  - +10 account: company.region in $home_countries
  - +15 contact: title contains head or title contains director or title contains vp
rubric: r-374ba4f0671031c6

lead <lead id>
email: tom.hale@ironbridge.example
name: Tom Hale
company: ironbridge.example
status: new
fit_signal: yes
tier: 4
score: 0 (account 0, contact 0)
reasons:
  - fit_signal = yes (rule 1: company.legacy_tool_seen = true)
  - tier = 4 (rule 2: company.employees < 50 or company.employees > 5000)
rubric: r-374ba4f0671031c6
```
