---
date: 2026-09-18
---

# Runners hold no schedules; a run may carry a start time

A runner holds nothing it did not claim — no cron, no loop, no queue of its own.
Loops, schedules and retries of a monitoring check are a hub's business, and a
hub makes runs. The protocol allows one thing in time: an optional one-shot
**start time** on a run, like an email API's `send_at`, which a hub may hand over
early so the run starts exactly on time. No recurrence.

## Considered options

**Local schedules on the runner**, so monitoring works with the hub down. It puts
a scheduler, persistence and catch-up on every machine and blurs who owns the
work; the operator ruled it out ("it should not even decide its own or hold its
own tasks").
