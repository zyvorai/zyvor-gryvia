# GryviaRdmaRetryHigh

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

`gryvia_fabric_rdma_retry_rate > 0.01` for 10 minutes (more than 1% of posted sends completed with a retry or RNR error).

## What it means

RDMA work requests are being retried, which stalls collectives. The 1% threshold is a placeholder.

## How to check

```bash
gryvia_nic_counter_rate{counter=~"out_of_sequence|packet_seq_err|local_ack_timeout_err|implied_nak_seq_err"}   # hardware retry counters per NIC
gryvia_fabric_nic_retry_rate    # same family, aggregated per node
On the node: `ibstat`, `rdma link show`, `ethtool -S <iface> | grep -i -E 'retry|timeout|out_of_seq'`
gryvia_rdma_completion_latency_seconds (histogram) for latency impact
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- Packet loss: dirty optics, bad cable, switch buffer overflow.
- PFC misconfigured or not enabled on the path (lossless class not lossless).
- Receiver not ready (RNR): slow receiver or too few posted receives.
- MTU mismatch along the path.

## Mitigation

- Reseat/replace the cable or optic on the port with rising counters.
- Verify PFC/ECN configuration end to end on NICs and switches (see GryviaPfcRateHigh, GryviaCnpRateHigh).
- Check MTU consistency.

## Limits (honest)

Needs the collector's RDMA probes and `-nic` counters; Gryvia has not been run on real RDMA hardware, so the mapping of counters to causes is documented reasoning, not field-tested.
