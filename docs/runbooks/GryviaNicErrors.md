# GryviaNicErrors

Severity: warning. Rule: [`monitoring/prometheus-rules.yaml`](../../monitoring/prometheus-rules.yaml).

## What fired

Sum of `symbol_error`, `link_downed`, `port_rcv_errors`, `port_xmit_discards`, `rx_discards_phy` rates for a device/port above 1 per second for 10 minutes.

## What it means

The NIC's hardware counters (from /sys/class/infiniband) show link-level errors or discards, which usually point at the physical layer.

## How to check

```bash
gryvia_nic_counter_rate{device="<dev>",port="<port>"}   # look at the counter label to see which counter
On the node: `rdma link show`, `ibstat`, `cat /sys/class/infiniband/<dev>/ports/<port>/counters/symbol_error`
`ethtool -m <iface>` for optics diagnostics (power levels, temperature)
```

To query the collector directly see [Calling a collector](README.md#calling-a-collector).

## Likely causes

- Dirty, damaged or badly seated fibre/DAC; failing transceiver.
- Link flapping (`link_downed` rising).
- Congestion-driven discards (`port_xmit_discards`, `rx_discards_phy`) rather than a physical fault.

## Mitigation

- Clean or replace the cable/optic; move the link to another port.
- For discards without symbol errors, treat it as congestion (see GryviaCnpRateHigh, GryviaPfcRateHigh).

## Limits (honest)

Which counters exist depends on the NIC driver; the collector exports a fixed list. A rate is per second averaged over the collector's sample interval.
