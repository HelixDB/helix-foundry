# Incident report: dispatch sync delays, 7 October 2026

**Severity:** SEV-1
**Incident commander:** Victor Osei, VP Engineering
**Status:** resolved; follow-ups open
**Written:** 9 October 2026

## Summary

Between 07:42 and 10:15 Pacific on 7 October 2026 (2 hours 33 minutes), changes made on the dispatch board did not reach technicians' mobile apps. 312 job updates across 11 customer accounts were delayed by up to 2 hours 20 minutes. Scheduling, the dispatch board itself and the rest of the mobile app kept working.

## Impact

The most affected accounts, by delayed job updates:

| Account | Delayed updates | Tickets |
| --- | --- | --- |
| Sable Solar | 96 | T-4401, T-4404 |
| Halvorsen Freight | 71 | T-4402 |
| Pinecrest Home Health | 58 | T-4403 |
| Corbel HVAC Services | 24 | T-4405 |
| Fenwick Fire Protection | 19 | T-4406 |

Six other accounts had fewer than ten delayed updates each and opened no tickets.

Under our service definitions the delay counts as downtime of the dispatch service. With no other downtime, the dispatch service's uptime for October 2026 is 99.66%. Customers whose order forms grant service credits are owed them at their order form's rates; Starter and Growth plans carry none.

## Cause

A release at 07:35 changed how the sync service acknowledges job updates. Under the morning load, two consumers of the update queue waited on each other's locks, and updates queued without being delivered. Our alerts watched errors, not lag, so nothing paged until customers called.

## Timeline (Pacific)

- 07:35: release 2026.10.07-1 deployed.
- 07:42: updates stop reaching the mobile apps.
- 07:51: first ticket, from Sable Solar.
- 08:20: support escalates to engineering; Victor Osei takes command.
- 09:58: release rolled back.
- 10:15: the queue drains; all updates delivered.

## Follow-ups

- Alert when sync lag passes 5 minutes. Owner: Victor Osei. Due 31 October.
- Load test the sync service at peak morning volume before each release. Owner: Victor Osei. Due 15 November.
- Credit notes to every customer whose order form grants credits. Owner: Elena Vasquez. Due 15 November.
