# GryviaCnpRateHigh

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

`gryvia_fabric_cnp_rate > 1000` CNP/s for 10 minutes.

## What it means

RoCEv2 congestion notification packets are arriving at a high rate: switches are ECN-marking and receivers are asking senders to slow down. Some CNP is normal under load; sustained high rates mean a persistently congested path. The 1000/s threshold is a placeholder: tune it against your baseline.

## How to check

```bash
rate(gryvia_roce_cnp_packets_total[5m]) / rate(gryvia_roce_packets_total[5m])   # CNP share of RoCE traffic
gryvia_nic_counter_rate{counter=~"np_cnp_sent|rp_cnp_handled|np_ecn_marked_roce_packets"}   # NIC-side view
Dashboard 'Gryvia / NIC, RDMA, PFC and CNP'
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- Incast or oversubscription on a leaf/spine link.
- ECN thresholds too low or too high on switches; DCQCN parameters mismatched.
- Uneven routing (ECMP hash collisions).

## Mitigation

- Inspect the switch port utilisation and ECN configuration for the affected nodes.
- Rebalance job placement across leaves; consider adaptive routing where available.

## Limits (honest)

The XDP program only counts packets it sees on the attached interfaces; it cannot tell which flow is congested.
