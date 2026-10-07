# Provisional event bodies

These Apollo workflow bodies are **provisional**. They are built from the shapes
in earlier working code's parser tests and contracts section 5.1, with made-up people and
example domains, not from real Apollo captures. Each file carries
`"provisional": true` (an unknown field the parsers ignore).

When S0 saves real bodies of each kind here, replace these files, drop the
`provisional` field, and update the expectations in
`adapters/apollo/golden_test.go`.

File names start with the receiver route that stores the body: `apollo_reply_`
(`POST /apollo/reply`) or `apollo_visit_` (`POST /apollo/visit`).
