# Multi-Tenant Isolation Test Scenarios

These tests validate the `isolation.d/` iptables mechanism introduced in dockyard v0.1.1.
They use the existing 3-instance test infrastructure (A=dy1, B=dy2, C=dy3).

## Prerequisites

All tests assume instances A, B, C are created and running (tests 01-07 have passed).

## Test Scenarios

### T1: Isolation chain created when rules file exists

Write a `.rules` file to instance A's `isolation.d/`, restart the service, and verify:
- The per-instance iptables chain `DOCKYARD-ISO-dy1` exists
- The chain contains ACCEPT rules for IPs listed in the rules file
- The chain ends with a DROP rule
- A FORWARD jump rule exists scoped to a `br-*` bridge

### T2: No isolation chain without rules files

Verify that instances B and C (which have no `isolation.d/*.rules` files) do NOT have
a `DOCKYARD-ISO-dy2` or `DOCKYARD-ISO-dy3` chain in iptables. The feature is opt-in.

### T3: Allowed IPs can communicate through the chain

On instance A:
1. Create a user-defined Docker network (`test-net`)
2. Start two containers on `test-net`: `infra` and `client`
3. Write `infra`'s IP to `isolation.d/infra.rules`
4. Restart the service (to apply rules)
5. Verify `client` can reach `infra` (ping or TCP connect)

### T4: Non-allowed IPs are blocked by the chain

Continuing from T3:
1. Start a third container `outsider` on the same `test-net`
2. `outsider`'s IP is NOT in any `.rules` file
3. Verify `outsider` CANNOT reach `client` (ping fails / times out)
4. Verify `outsider` CAN still reach `infra` (infra IP is in the allow-list)

### T5: Isolation chain cleaned up on stop

After T3/T4:
1. Stop instance A's service
2. Verify `DOCKYARD-ISO-dy1` chain no longer exists in iptables
3. Verify no FORWARD rules reference `DOCKYARD-ISO-dy1`
4. Start instance A again
5. Verify the chain is recreated (rules file still present)

### T6: Multiple rules files merged

On instance A:
1. Write `isolation.d/infra.rules` with IP-1
2. Write `isolation.d/extra.rules` with IP-2
3. Restart service
4. Verify the chain contains ACCEPT rules for both IP-1 and IP-2

### T7: Comments and blank lines in rules files ignored

Write a `.rules` file containing:
```
# This is a comment
10.0.0.1

  # Indented comment
10.0.0.2
```
Restart, verify only `10.0.0.1` and `10.0.0.2` appear as ACCEPT rules (no `#` artifacts).

### T8: Cross-instance isolation preserved with isolation.d

Enable isolation on instance A. Verify containers from A still cannot see containers
from B (the existing test 13 behavior), and vice versa. The isolation.d feature
should not interfere with daemon-level separation.

### T9: Destroy cleans up isolation chain and rules

Destroy instance A. Verify:
- `DOCKYARD-ISO-dy1` chain is gone from iptables
- `${DOCKYARD_ROOT}/etc/isolation.d/` directory is gone (removed with the instance root)

### T10: Isolation survives reboot

Enable isolation on instance A, reboot, verify:
- Service starts automatically
- `DOCKYARD-ISO-dy1` chain is recreated from persisted rules files
- ACCEPT/DROP rules match what was written before reboot
