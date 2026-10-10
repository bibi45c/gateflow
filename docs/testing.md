# Verification

## Checks

No application stack, automated checks or required CI gates are configured yet.
Agree the application checks before implementation; add exact commands and mandatory
gates here when their configuration is committed. No configured check means no
automated evidence, not a passed result.

For each change, run the closest meaningful checks, then all applicable mandatory
checks. Governance/template changes also need a realistic case; syntax alone does
not establish behavior. Isolate resources that concurrent tests can modify.

## Evidence

Record command/scenario, environment, head/base or candidate, tested inputs and
configuration, result and concise evidence. Results: passed, failed, not run, blocked;
explain the last three. Identify tested uncommitted inputs in delivery.

Revalidate affected checks after content/base changes or uncertain equivalence.
Independent branch passes do not prove their integration.

## Gates

Separate required pre-merge checks from approved post-merge acceptance. Required
failures or missing checks block merge; Done requires all agreed acceptance recorded.
Post-merge acceptance cannot defer required security, contract or correctness checks.

Local checks, CI, agent review, human approval, merge and deployment are distinct
evidence. Inspect actual repository protections; this document does not enable them.
