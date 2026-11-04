# Behaviour study: how students act on a slow result page

Gap G3 in [`../reports/next-paper-gap-analysis.md`](../reports/next-paper-gap-analysis.md).
Every result about refreshes and waiting rooms in the first paper rests on an
assumed student model: patience 5–20 s, 80–90% retry, a manual refresh every
minute or two in a queue, no limit on time in a queue. No published study
measures any of these for result pages. This study measures them with a mock
result portal and feeds the numbers back into ResultBench (`users:` in a
scenario file).

It is deliberately small: one lab session per batch of students, on their own
phones over the lab Wi-Fi, about 10 minutes each.

## Research questions

1. When a result page hangs, how long do students wait before reloading or
   leaving? (`patience_s`, `impatient_retry_prob`)
2. After an error page, how often and how soon do they try again?
   (`retry_prob`, `retry_delay_s`)
3. In a waiting room that says "do not refresh", how often do they refresh
   anyway, and how long do they stay? (`queue_manual_refresh_per_min`,
   `queue_patience_s`)
4. Does showing an estimated wait change how long they stay? (`eta_balk`;
   compare `queue` with `queue-eta`)

## Design

Between subjects, four conditions, assigned from the participant code
(balanced by a hash; or set `&c=` on the link to balance by hand):

| Condition | What the participant sees |
|---|---|
| `slow` | The page spins; each request fails with 504 after 25 s |
| `error` | Each request fails with 503 after 1–3 s |
| `queue` | Waiting-room screen with a queue position, updated every 5 s |
| `queue-eta` | As `queue`, plus "Estimated wait: about N minutes" |

In every condition the result appears 4 minutes after the first request
(`--release-after`). Participants can stop at any time with "I would stop here
and check later", and then answer four questions (reload count, queue
tolerance, what they would do next, past experience).

Target size: 20 per condition (80 students). Waiting times are right-censored
at 4 minutes, so the analysis uses Kaplan–Meier estimates.

## Running a session

```bash
python3 server.py --port 8000 --log events-2026-12-session1.jsonl
```

Give each student a slip with a link `http://<laptop-ip>:8000/?p=<code>`
(codes such as `S1-017`; keep no list linking codes to names). Read the
instructions below aloud, then let everyone start at once.

Afterwards:

```bash
python3 analyze.py events-2026-12-session1.jsonl --yaml suggested-users.yaml
```

The YAML lists suggested values for ResultBench's student model. Check them
against the per-condition tables before using them in `bench/scenarios/`.

## Instructions to read aloud

> This is a study about how people use result websites. The website you will
> open is a mock: no real result and no personal data. Imagine today is your
> board result day and use the website the way you really would: wait, reload,
> or stop. There is no right or wrong behaviour and nothing depends on how long
> you stay. You can stop at any time with the button on the page. At the end
> there are four short questions.

## Consent and data

- Participation is voluntary; students may stop at any time without giving a
  reason, and this has no effect on marks or attendance.
- No names, roll numbers, phone numbers or IP addresses are stored. The roll
  number typed into the form is never sent to the server. The log holds the
  anonymous code, the condition, event types and times, and the survey answers.
- Students under 18 need guardian consent under the institute's rules; run the
  study with M.Tech/B.Tech students who are adults unless approved otherwise.
- Get the mentor's sign-off, and the department ethics committee's if
  required, before the first session (plan section 10).

Consent slip text:

> I agree to take part in a 10-minute study on how students use result
> websites. I understand that the website is a mock, that no personal data is
> collected, and that I can stop at any time.

## Limits

- A mock result in a lab is not result-day anxiety: real students are likely
  more persistent. Report the numbers as lower bounds on patience and use them
  as one end of the sensitivity range, not as the truth.
- Phones over lab Wi-Fi, not mobile data.
- One condition per student; no within-subject comparison.

## Files

| File | What |
|---|---|
| `server.py` | Mock portal and event log (standard library only) |
| `static/index.html` | The page participants see |
| `analyze.py` | Per-condition statistics and suggested `users:` parameters |
| `test_study.py` | Tests: `python3 -m pytest behaviour-study` |
