# Apollo workflow templates

Each file here is the body of one Apollo workflow's "send webhook" action. The
receiver (`leadscore serve`) stores every request it accepts, and the next run
reads it. Create one workflow per file you need.

> **Keep the receiver secret private.** It works like a password: anyone who
> has it can send fake events, including a fake positive reply that a deal
> lane would act on. Never paste it into chat, tickets or shared docs; type it
> only into the Apollo workflow and your `.env` (or Secret Manager). Rotate it
> if it might have leaked (see the main README).

## Every workflow

- **Method and URL:** `POST` to your receiver's address, `receiver.public_url`
  in `leadscore.yml`, plus the route below. On Google Cloud that is the
  `leadscore-receiver` service's URL; on a server, your domain behind Caddy;
  on a laptop, your tunnel's URL.
- **Header:** `X-Leadscore-Secret: <your receiver secret>`. If Apollo cannot
  send a custom header, add the secret to the body instead as a top-level
  field, `"leadscore_secret": "<your receiver secret>"`. The receiver removes
  it before storing. Never put the secret in the URL: URLs are logged.
  Prefer the header: a body secret is only known once the body is read, so
  the receiver reads such bodies a few at a time, and a flood of slow
  senders can delay them (Apollo then retries); header requests are never
  delayed this way.
- **Body:** the template, with each `<...>` value replaced by the Apollo
  variable it names, picked in Apollo's workflow editor. Keep the JSON keys
  and the `event` value exactly as written: the event value is what tells
  leadscore what happened. Leave out a field Apollo has no variable for.

The receiver answers `200` once the event is stored, `401` for a wrong or
missing secret, and `503` when it could not store the event in time, so
Apollo can send it again. A repeated event is counted once.

## Reply workflows: `POST /apollo/reply`

| File | Attach the workflow to | What leadscore does |
|---|---|---|
| `email_sent.json` | a sequence email was sent | marks the person contacted |
| `email_replied.json` | any reply | records a neutral reply |
| `email_replied_positive.json` | a reply your workflow's filter judges positive | records a positive reply, which a non-cold lane can act on |
| `email_unsubscribed.json` | the person opted out | marks them unsubscribed: never contacted again |

Use `email_replied_positive` only on a workflow whose trigger keeps just the
positive replies; a workflow that fires on every reply must use
`email_replied`. With `replies: polling` in `leadscore.yml`, replies are read
from Apollo instead, and the two reply workflows are not needed (sent and
unsubscribed still are).

## Website visit workflows: `POST /apollo/visit`

`website_visited.json` is for one page or page group. Change
`website_visited_pricing` to `website_visited_<name>`, where `<name>` is
lowercase letters, digits and `_`; leadscore reads it as the event
`visit_<name>`, which your rubric's detectors count. Make one workflow per
name, and list every `visit_<name>` in `receiver.visit_events`, so a workflow
that goes quiet is noticed.

## What still needs checking

These templates follow the body formats in the design (contracts section
5.1). Which Apollo variables can fill each field, whether a workflow can send
a custom header, and how long Apollo waits and whether it retries are being
checked against a real Apollo account; this page will be updated with the
answers.
