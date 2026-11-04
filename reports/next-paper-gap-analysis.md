# Gap analysis for the next paper

Date: 4 Nov 2026. Builds on the first paper (`paper/main.tex`, `paper/dasa26.tex`),
the research plan (`result-portal-research-plan.md`) and the earlier check
(`reports/Result portal gap analysis check.md`). This document does not repeat
those; it asks what is still open after paper 1 and which piece of it makes the
best next paper.

## 1. Bottom line

Paper 1 answered the lab questions well enough that a second lab comparison of
the same eleven designs would add little. Three gaps are both real and
reachable before the thesis is due:

1. **No public measurement of result day exists** (plan Study 1). This is still
   the largest gap, and searches in this check again found no availability or
   traffic study of Indian result or e-governance portals, only accessibility
   and usability audits. It is tied to the Feb–Jun 2027 result season and
   cannot be re-run until 2028.
2. **A waiting room that works for result day is an open design problem.** Paper 1
   showed that both of our waiting rooms had a failure mode: naive tickets
   punished students who refreshed, and token tickets let admitted, impatient
   students overload the backend (74.5% served vs 98.3%). No peer-reviewed
   evaluation of user-facing waiting rooms other than WebQ (2015) was found,
   then or now. This can be done in the lab now, with the existing code.
3. **How students actually behave on a slow result page is unmeasured.** Every
   paper-1 conclusion about refreshes and waiting rooms rests on assumed
   patience and refresh probabilities, and paper 1's own sensitivity results
   show the conclusions swing with them (amplification 1.17 → 2.23 → 8.65
   across student types). No study measuring human refresh under overload
   was found.

**Recommendation:** make the next paper the **waiting-room design paper (option B
below), anchored by a small student-behaviour study (option C)**, and start the
**field study (option A)** in parallel this month, because its RTI requests,
ethics sign-off and monitor deployment have to happen before February. Option A
then becomes paper 3 (Jun–Jul 2027).

## 2. What paper 1 settled (do not redo)

| Question | Answer in paper 1 | Residual value of more work |
|---|---|---|
| Which architecture scales? | Pre-computed (C1 Redis, C2 static) served 100% at 10× with 12.5–57.8× less CPU per student; A fell to 30.8% at 2× | Low. Ordering is near-true by construction; margins are already lower bounds |
| Mirrors? | M no better than A on the same budget | Low |
| Is cold cache-aside a stampede? | No: almost every miss is a first-time key; coalescing and XFetch did nothing; pre-warming fixed it | Low. Clean result, already published |
| Wrong results under load? | None in any design; stale only from un-invalidated copies | Low for lab. The real correctness risks (WAEC 2025, CBSE 2026 evaluation errors) are in result *computation*, outside the serving path |
| Do refreshes amplify load? | Yes, 2.23 req/student by default, 8.65 for impatient students; impatient students kept A overloaded | Medium: the numbers depend on an unmeasured user model (see gap 3) |
| Waiting room? | Served 98.3% vs 43.3% at 5×, but median wait 121 s; tokens without a per-admit limit made it worse | **High**: it showed the problem, not a design that solves it |

## 3. What is still open

Sources: paper 1's threats and conclusion (`paper/sections/threats.tex`,
`conclusion.tex`), plan sections 6, 8, 9 and 11, and incidents found in this check.

| # | Gap | Where it comes from | Already in repo | Novelty after this check |
|---|---|---|---|---|
| G1 | No measurement of real result-day load, failure onset, duration, recovery | Plan RQ1/C1; paper 1 threat "we have not measured a real result day" | `monitor/` (built, never run), `docs/field-study.md`, `docs/rti-template.md` | **Strong.** Searches found Indian e-gov studies only on accessibility/usability; one 2016 CIS audit noted ~25% of 7,800 government sites unavailable at test time, but no surge measurement |
| G2 | No waiting-room design that keeps both fairness and backend protection under refresh | Paper 1 RQ2 findings | `internal/wroom/` (FIFO/random × naive/token) | **Strong.** Only vendor material and WebQ (2015); a 2026 vendor piece reports 31% bot traffic in one waiting room, which no study addresses |
| G3 | Human refresh/patience/abandonment behaviour is assumed | Paper 1 threat "Modelled students"; plan threat 2 | Parameterised user model in `internal/surge/` | **Strong.** Closest work: a 2020 study of faulty web interactions (users retried ~2–5 times) and a ride-sharing virtual-queue field experiment (Management Science) showing wait-time estimates change abandonment. Nothing for result pages or flash crowds |
| G4 | Capacity-planning model never validated or published | Plan Study 3/C6 | `model/capacity.py` (fluid model, `fit` and `plan` commands) | Medium. Queueing models of overload with abandonment exist; a validated, board-facing guide does not |
| G5 | Single machine, no CDN, no multi-node | Paper 1 conclusion | Compose files only | Medium–low. Engineering extension; novelty comes from cost (₹ per result day) and validating the 1-machine conclusions at scale |
| G6 | Authentication, captcha, sessions and write paths are out of scope | Paper 1 threat "Scope"; plan section 11 | None | **Strong but costly.** New 2026 evidence: CBSE's re-evaluation portal (opened 2 Jun 2026) had login expiries, "verification failed" errors, an added Aadhaar check, and claimed DoS traffic (reported as 1.5 million hits in 2 min); the Quebec 2026 outage was a login failure; real Indian result pages need roll number + DOB or captcha |
| G7 | Alternative channels (DigiLocker, UMANG, SMS, mirrors) and how much load they absorb | Plan section 2.1; earlier check | None | Medium–strong, measurable only in the field. Natural experiment: KNEC scrapped SMS results for 2025 KCSE (released 9 Jan 2026) and the portal went down within the hour |
| G8 | Bots and DoS vs genuine crowd on result day | Paper 1 threat "Scope"; CBSE 2026 | None | Medium. Flash crowd vs DoS separation is classic (Jung et al. 2002); the result-day version with a scheduled crowd is unstudied, but data is hard to get |
| G9 | Loose ends inside paper 1 | `docs/experiments.md` vs `paper/generated/` | Grids exist | Not a paper on their own. The `faults` grid was never run; D-C2 unused; core grid stops at 10× (plan said 20×); 3 repetitions instead of the planned 5. Good material for a journal extension of paper 1 |

## 4. Candidate next papers

### Option A: Field study, "What result day looks like" (G1, G7)

- **Question.** When do Indian result portals fail, for how long, and do
  hosting and alternative channels make a difference?
- **Method.** Run `monitor/` from two vantage points for 8–12 releases in
  Feb–Jun 2027; record DNS, TTFB, status, content fingerprint, CDN and hosting
  hints. File RTI requests for peak request rates. If AKTU agrees, get
  aggregate access-log counts for one semester release; that alone would give
  the first real result-day arrival curve.
- **Quick win available now:** a *hosting census* of the ~30 result portals,
  boards, DigiLocker and UMANG (who hosts, CDN or not, IPv6, TLS, DNS TTLs).
  It needs no result day, de-risks the monitor, and is a section of the paper.
- **Why new.** No public result-day measurement exists. KNEC's dropping SMS in
  2026, and Indian boards pointing students to DigiLocker, UMANG and SMS, give a
  channel comparison no-one has made.
- **Risks.** An outside monitor sees availability, not request rates; RTI
  replies may be refused or late; few events may fail visibly in one season.
  Ethics sign-off needed even though the monitor is light.
- **Timing.** Hard deadline: monitor live by early Feb 2027. Data complete
  around June; paper in July.
- **Venue type.** Internet measurement (IMC short paper, PAM), or e-governance
  (ICEGOV), or Indian systems venues (COMSNETS). Check current deadlines.

### Option B: Waiting rooms for scheduled surges (G2, with G3 as input) — recommended next

- **Question.** Can a waiting room serve almost everyone *and* keep arrival
  order *and* keep the backend safe when students refresh, and at what wait
  cost?
- **What to build (in `internal/wroom/`).**
  1. *Admission leases:* an admitted student gets a token valid for k requests or
     t seconds, which directly closes the failure that cost tokens 98.3% → 74.5%.
  2. *Closed-loop admission:* set the admission rate from backend latency or
     queue length (Breakwater-style control applied to humans), instead of a fixed 80% of calibrated
     capacity.
  3. *Pre-release randomisation vs FIFO vs lottery-then-FIFO*, with fairness
     measured as Kendall's τ and as the spread of waits.
  4. *Wait-time estimates:* show an ETA and model abandonment as a function of
     it, using the ride-sharing evidence and option C's data.
  5. *Bot pressure:* a share of automated clients that never give up, to test
     whether the waiting room protects real students.
- **Baselines.** Paper 1's naive and token designs, A alone, A with double budget, C1.
- **Why new.** No peer-reviewed evaluation of user-facing waiting rooms beyond
  WebQ; paper 1 already showed the obvious designs fail, so a fix is a clear
  contribution rather than a re-measurement.
- **Risks.** Still a lab study with a modelled user; mitigated by option C.
  An examiner may ask "why not just pre-compute?" — answer: write paths and
  authenticated flows (G6) cannot be pre-computed, so make one experiment put the
  waiting room in front of a write-heavy or login-bound backend.
- **Timing.** Nov 2026–Feb 2027, all on existing hardware.
- **Venue type.** Performance and systems (ICPE, Middleware, CNSM/NOMS, IEEE TNSM).

### Option C: How students behave on a slow result page (G3)

- **Question.** How long do students wait, how often do they refresh, and when
  do they give up, with and without a queue position or ETA?
- **Method.** A browser experiment with consenting AKTU students: a mock result
  page with injected delays, errors and a waiting-room screen; record
  refresh and abandonment times. Optionally a short survey on past result days.
  No real results, no load on real portals.
- **Why new.** No measurement of human refresh under overload was found. It
  converts paper 1's biggest threat into data and gives both option B and the
  capacity model real parameters.
- **Risks.** Lab behaviour is not result-day anxiety; needs ethics approval;
  sample size. Best as a section of option B, not a standalone paper,
  unless the effect sizes are striking.
- **Timing.** 4–6 weeks including approval.

### Option D: Beyond the lookup — login, captcha and write surges (G6, G8)

- **Question.** What happens when the surge hits an authenticated or
  write-heavy path (re-evaluation applications, counselling choice filling,
  payment), where pre-computation cannot help?
- **Why new.** Paper 1's answer ("pre-compute") does not apply; CBSE 2026 and
  Quebec 2026 show these paths fail in practice.
- **Risks.** Large new build (sessions, captcha cost, payment stubs, writes with
  conflicts); harder to keep designs comparable.
- **Timing.** 3–4 months. Better as the paper after the thesis, or a smaller
  experiment inside option B.

### Option E: Validated capacity guide and scale-out (G4, G5)

- **Question.** Given candidates, design and target, how many cores are
  needed, does the one-machine ordering hold on several machines and behind a
  CDN, and what does result day cost in rupees?
- **Why new.** No planning guide exists; but queueing models with abandonment
  are well known, so the novelty is the validation and the board-facing table.
- **Risks.** Needs cloud credits; reviewers may see it as engineering.
- **Timing.** 6–8 weeks. Fits best as the closing chapter of the thesis or
  part of option A's paper (field data validates the model).

## 5. Comparison

| | A Field | B Waiting room | C Behaviour | D Login/writes | E Capacity |
|---|---|---|---|---|---|
| Strength of gap | High | High | High | High | Medium |
| Builds on existing code | Monitor ready | wroom ready | New small app | Mostly new | Model ready |
| Can start now | Prep only | Yes | After approval | Yes | Yes |
| Calendar risk | Tied to season | Low | Ethics | Scope creep | Cloud credits |
| Standalone paper? | Yes | Yes | Weak alone | Yes | Weak alone |
| Feeds the thesis plan | Study 1 | Study 2 extension | Study 2 threats | Future work | Study 3 |

## 6. Suggested sequence

| When | Work |
|---|---|
| Nov 2026 | File RTI requests (template ready); ethics sign-off for monitor and behaviour study; ask AKTU IT cell for log aggregates; run the hosting census |
| Nov–Dec 2026 | Option B designs 1–3 in `internal/wroom/`; behaviour-study mock page |
| Jan 2027 | Behaviour study with students; option B grid; monitor dry run on a semester release |
| Feb 2027 | Submit option B paper; monitor live for the season |
| Feb–Jun 2027 | Field season (option A); in parallel run paper 1's `faults` grid and 5-rep reruns for a journal extension |
| Jun–Jul 2027 | Fit surge model and validate `model/capacity.py` against field data (option E); write option A paper |

## 7. Novelty checks still to do by hand

This check used general web search only. arxiv.org and news.careers360.com were
blocked from this session, so several items are from search snippets.

- Read **arXiv 2606.00942** ("Characterizing Metastable Faults and Failures",
  2026), which argues that retry storms come from a feedback loop between
  amplification and capacity degradation, and **arXiv 2608.25403** (retry
  amplification factor, Aug 2026). Neither appears to model human refresh,
  but confirm before claiming G3.
- Search IEEE Xplore, ACM DL and Scopus for "virtual waiting room", "online
  queue" AND evaluation, and for WebQ (Doshi et al. 2015) citations; any
  2025–26 follow-up decides how option B is worded.
- Search for "website availability" OR "outage" AND (India OR e-government)
  measurement studies at IMC, PAM and TMA to confirm G1.
- Verify the CBSE re-evaluation figures (1.5 million hits in 2 minutes is
  the Free Press Journal headline; another search summary said 15 million;
  Medianama's timeline says 3.8 million packets) before citing any of them.
- Confirm the KNEC 2026 SMS withdrawal from Capital FM / The Standard text.

## 8. Sources found in this check

- CBSE re-evaluation portal, 2026: [Free Press Journal](https://www.freepressjournal.in/education/cbse-re-evaluation-portal-survives-cyberattack-scare-after-15-million-hits-emerge-within-2-minutes-over-16000-students-complete-submissions), [Outlook](https://www.outlookindia.com/national/cbse-re-evaluation-portal-goes-live-after-delay-board-claims-cyberattack-attempts), [Deccan Chronicle](https://www.deccanchronicle.com/southern-states/telangana/cbse-re-evaluation-portal-goes-live-amid-glitches-failed-logins-and-student-anxiety-1960884), [Odisha Bytes](https://odishabytes.com/cbse-extends-session-time-after-students-report-login-errors-on-recheck-portal/amp/), [Medianama timeline](https://www.medianama.com/2026/06/223-cbse-2026-evaluation-crisis-timeline/)
- 2026 board result outages: [MP Board, Free Press Journal](https://www.freepressjournal.in/indore/mpbse-results-2026-websites-crash-lakhs-of-students-left-waiting-as-anxiety-peaks), [HPBoSE, The Logical Indian](https://thelogicalindian.com/himachal-pradesh-hpbose-class-10-results-2026-declared-at-11-am-website-crashed-amid-rush-marksheets-available-on-digilocker/), [UK Board, NewsX](https://www.newsx.com/education/uk-board-result-2026-result-not-loading-or-site-down-heres-how-students-can-check-results-at-ubseukgovin-204909/)
- KNEC 2026: [Capital FM, SMS scrapped](https://www.capitalfm.co.ke/news/2026/01/how-to-access-your-2025-kcse-results-as-sms-scrapped/), [Education News, portal glitch](https://educationnews.co.ke/?p=495721), [The Standard](https://www.standardmedia.co.ke/national/article/2001538147/how-2025-kcse-candidates-can-check-their-results)
- Quebec 2026: [CTV News](https://www.ctvnews.ca/montreal/article/quebec-students-and-parents-unable-to-view-exam-scores-online-after-portal-difficulties/)
- Metastability and retries: [arXiv 2606.00942](https://arxiv.org/html/2606.00942v2), [arXiv 2608.25403](https://arxiv.org/abs/2608.25403), [arXiv 2510.03551](https://arxiv.org/pdf/2510.03551)
- User behaviour: [Faulty web interactions study](https://www.researchgate.net/publication/346385035_Research_on_User_Behaviors_and_Tolerance_of_Faulty_Web_Interactions), [Virtual-queue delay information field experiment, Management Science](https://doi.org/10.1287/mnsc.2022.4448)
- Waiting rooms (vendor only): [Retail TouchPoints, bots in queues](https://www.retailtouchpoints.com/executive-viewpoints/agents-get-in-line/621245), [Queue-it buyer's guide](https://queue-it.com/blog/how-to-choose-virtual-waiting-room/)
- Indian e-gov web studies (accessibility only): [CIS 2016 report](https://cis-india.org/accessibility/accessibility-of-govt-websites.pdf), [Springer UAIS 2022](https://link.springer.com/article/10.1007/s10209-021-00800-8)
