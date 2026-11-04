# November 2026 checklist

What has to happen this month so that both the waiting-room paper (option B)
and the field study (option A) stay on schedule. From the sequence in
[`next-paper-gap-analysis.md`](next-paper-gap-analysis.md), section 6.

Items marked **(you)** need a person: a signature, a post office, a meeting
or a phone. Everything else is in the repository and ready to run.

## Field study (season starts February 2027)

- [ ] **(you)** Fill in `resultbench/docs/rti-template.md` and file RTI
  requests with NIC, CBSE, NTA, AKTU and two or three state boards (UP, Bihar,
  MP). Replies are due within 30 days, so filing by mid-November gets answers
  before January. Keep the postal receipts or online acknowledgement numbers in
  `field/rti/` (not in git if they contain personal details).
- [ ] **(you)** Ask the AKTU IT cell, through the mentor, for per-second
  request counts and status codes (no roll numbers, no IPs) for one past and
  one upcoming semester result release (plan section 6.3).
- [ ] **(you)** Mentor sign-off on the monitor and the behaviour study; ask
  whether the department ethics committee must see them (plan section 10).
- [ ] Set `contact` in `resultbench/monitor/targets.yaml` to your name and
  university address (it is sent in the User-Agent so a site owner can reach
  you), check every URL still works, and add DigiLocker/UMANG result pages and
  mirror links as boards announce them.
- [ ] Run the hosting census once (one request per target, robots.txt
  respected):

  ```bash
  cd resultbench && python -m monitor.census --config monitor/targets.yaml --out field/census-2026-11.json
  ```

  Repeat in January and after the season: changes in hosting or CDN use
  between rounds are themselves a result.
- [ ] Rent or set up the second vantage point (a small VM in an Indian
  region) and keep its clock synchronised.

## Behaviour study (December–January)

- [ ] **(you)** After sign-off, book a lab slot and print code slips
  (`S1-001` … `S1-080`); see `behaviour-study/README.md`.
- [ ] Do a dry run with two or three friends first; check that phones on the
  lab Wi-Fi reach the laptop (`python3 behaviour-study/server.py`).
- [ ] After each session: `python3 behaviour-study/analyze.py <log> --yaml suggested-users.yaml`.

## Waiting-room paper (lab, now to February)

- [ ] Calibrate design A on your machine (the 8-core desktop used for paper 1)
  and run the grids; about 14 hours of machine time in total:

  ```bash
  cd resultbench && bash bench/paper2-run.sh
  ```

- [ ] When the behaviour-study numbers are in, re-run `p2-eta` and
  `p2-loads` with the measured `users:` values as a sensitivity check.
- [ ] Check deadlines for the target venue (ICPE, Middleware, CNSM/NOMS or
  IEEE TNSM) and fix the submission date.
