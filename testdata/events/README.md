# Provisional event bodies

These Apollo workflow bodies are **provisional**. They are built from the shapes
in earlier working code's parser tests and docs/reference.md ("Receiver"), with made-up people and
example domains, not from real Apollo captures. Each file carries
`"provisional": true` (an unknown field the parsers ignore).

Some keys are backed by records of our own Apollo account, and each body
says which in a `confirmed_from` field (also ignored by the parsers):

- Reply bodies: the workflow setup shows `event` (a literal typed into each
  workflow), `contact_email`, `contact_stage` and `last_conversation_link`.
  The variable catalogue seen on 2026-08-19 lists no contact id (another
  token is not ruled out), so no reply body or template carries
  `contact_id`; replies are matched by email. `contact_name`,
  `contact_title`, `contact_linkedin_url` and `account_domain` are still
  unconfirmed.
- Visit bodies: the workflow setup shows the per-domain `event`, the contact's
  email, first and last name, title and LinkedIn URL, and an `account` block.
  Still open: the account block's `domain`, `website_url` and `name`,
  `contact.id`, and `visited_at` (its values here are made up).

Once real bodies of each kind are captured, replace these files, drop the
`provisional` field, and update the expectations in
`adapters/apollo/golden_test.go`.

File names start with the receiver route that stores the body: `apollo_reply_`
(`POST /apollo/reply`) or `apollo_visit_` (`POST /apollo/visit`).
