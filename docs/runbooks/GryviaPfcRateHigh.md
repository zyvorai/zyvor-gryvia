# GryviaPfcRateHigh

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

`gryvia_fabric_pfc_rate > 100` pause frames/s for 10 minutes.

## What it means

The node is receiving many 802.1Qbb priority flow control pause frames. PFC makes the fabric lossless but sustained pausing propagates head-of-line blocking and can escalate to a PFC storm or deadlock. The threshold is a placeholder.

## How to check

```bash
rate(gryvia_pfc_pause_frames_total[5m]) and sum by (priority) (rate(gryvia_pfc_priority_pause_frames_total[5m]))   # which priority is being paused
rate(gryvia_pfc_legacy_pause_frames_total[5m])   # 802.3x global pause: usually a misconfiguration
On the node: `ethtool -S <iface> | grep -i pause`; on the switch: PFC watchdog counters
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- A receiver that cannot drain fast enough (slow host, PCIe, or a hung NIC).
- Congestion that ECN does not resolve (see GryviaCnpRateHigh).
- Legacy 802.3x pause enabled together with PFC.

## Mitigation

- Identify the paused priority and the sender at the far end via switch counters.
- Enable/verify the switch PFC watchdog; disable 802.3x global pause if enabled.
- Restart or drain the node if a NIC is stuck sending pauses.

## Limits (honest)

Counts frames on the interfaces the XDP program is attached to; it does not identify the pausing peer.
