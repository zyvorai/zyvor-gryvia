import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import { api } from '@/lib/api'
import { notify } from '@/lib/notify'
import { useIsAdmin } from '@/lib/useRole'
import { saveBlob } from '@/lib/download'
import { formatNumber } from '@/lib/format'
import { formatRate } from '@/lib/cloud'
import { classLabel, formatMoney, invoiceActions, invoiceBadge, invoiceFilename, voidReasonError, networkRateDisplay, invoiceParams, invoiceTotalDisplay, invoiceTotalLabel, monthLabel, parseMonth, periodLabel, type Invoice } from '@/lib/invoices'
import { useDocumentTitle } from '@/hooks/useDocumentTitle'
import PageHero from '@/components/PageHero'
import ConfirmDialog from '@/components/ConfirmDialog'
import { EmptyState, ErrorState, Skeleton } from '@/components/StateViews'

export default function Invoices() {
  useDocumentTitle('Invoices')
  const admin = useIsAdmin()
  const [params, setParams] = useSearchParams()
  const month = useMemo(() => parseMonth(params.get('month')), [params])
  const tenant = admin ? (params.get('tenant') ?? '') : ''
  const [busy, setBusy] = useState<string | null>(null)
  const [finalizing, setFinalizing] = useState<Invoice | null>(null)
  const [voiding, setVoiding] = useState<Invoice | null>(null)
  const [reason, setReason] = useState('')
  const queryClient = useQueryClient()

  const setParam = (name: string, value: string) =>
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        if (value) next.set(name, value)
        else next.delete(name)
        return next
      },
      { replace: true },
    )

  const tenants = useQuery({ queryKey: ['tenants'], queryFn: api.getTenants, enabled: admin })
  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: ['invoices', month, tenant],
    queryFn: () => api.getInvoices(invoiceParams(month, tenant)),
  })

  const finalize = useMutation({
    mutationFn: (inv: Invoice) => api.finalizeInvoice(inv.tenant, month),
    onSuccess: (inv) => {
      notify.success(`Finalized ${inv.number}`)
      setFinalizing(null)
      return queryClient.invalidateQueries({ queryKey: ['invoices'] })
    },
    onError: (err) => notify.error('Could not finalize the invoice', err),
  })
  const voidInv = useMutation({
    mutationFn: (inv: Invoice) => api.voidInvoice(inv.tenant, month, reason.trim()),
    onSuccess: (inv) => {
      notify.success(`Voided ${inv.number}`)
      setVoiding(null)
      setReason('')
      return queryClient.invalidateQueries({ queryKey: ['invoices'] })
    },
    onError: (err) => notify.error('Could not void the invoice', err),
  })
  const reasonError = voidReasonError(reason)
  const ledger = !!data?.billingLedger

  const download = async (inv: Invoice, format: 'csv' | 'json') => {
    const key = `${inv.number}:${format}`
    setBusy(key)
    try {
      const blob = await api.downloadInvoice(inv.tenant, month, format)
      saveBlob(blob, invoiceFilename(inv.tenant, month, format))
    } catch (err) {
      notify.error(`Could not download ${format.toUpperCase()}`, err)
    } finally {
      setBusy(null)
    }
  }

  return (
    <>
      <PageHero
        eyebrow="Invoices"
        title={admin ? 'Invoices.' : 'Your invoices.'}
        lede={
          ledger
            ? 'Monthly statements built from job run time and the catalog rates. A finalized invoice is frozen from the hash-chained billing ledger; until then it is an estimate.'
            : 'Monthly statements built from job run time and the catalog rates. These are estimates, not bills; nothing is charged.'
        }
      />
      <div className="grid">
        <section className="card span3">
          <p className="eyebrow">Filters</p>
          <div className="toolbar">
            <label>
              Month
              <input type="month" value={month} onChange={(e) => setParam('month', e.target.value)} />
            </label>
            {admin && (
              <label>
                Tenant
                <select value={tenant} onChange={(e) => setParam('tenant', e.target.value)}>
                  <option value="">All tenants</option>
                  {(tenants.data ?? []).map((t) => (
                    <option key={t.metadata.name} value={t.metadata.name}>
                      {t.spec?.displayName || t.metadata.name}
                    </option>
                  ))}
                </select>
              </label>
            )}
          </div>
        </section>

        {isLoading ? (
          <section className="card span3">
            <Skeleton rows={3} />
          </section>
        ) : isError ? (
          <div className="span3">
            <ErrorState title="Could not load invoices." error={error} onRetry={() => refetch()} retrying={isRefetching} />
          </div>
        ) : data && data.items.length === 0 ? (
          <div className="span3">
            <EmptyState title={`No usage in ${monthLabel(month)}`}>Invoices appear here once jobs have run in the selected month.</EmptyState>
          </div>
        ) : (
          (data?.items ?? []).map((inv) => {
            const badge = invoiceBadge(inv.status)
            const actions = invoiceActions(inv, { admin, ledger })
            return (
            <section key={inv.number} className="card span3" aria-labelledby={`inv-${inv.number}`}>
              <p className="eyebrow">{periodLabel(inv.period)}</p>
              <h2 className="card-title" id={`inv-${inv.number}`}>
                <span className="mono">{inv.number}</span> · {inv.tenant}
              </h2>
              <p>
                <span className={badge.tone ? `pill ${badge.tone}` : 'pill'}>{badge.label}</span> <span className="faint">{inv.currency}</span>
                {inv.open && <span className="faint"> Includes running jobs; the amount can still change.</span>}
                {inv.finalizedAt && <span className="faint"> Finalized {inv.finalizedAt.slice(0, 10)}{inv.finalizedBy ? ` by ${inv.finalizedBy}` : ''}.</span>}
                {inv.paidAt && <span className="faint"> Paid {inv.paidAt.slice(0, 10)}.</span>}
                {inv.ledger && inv.ledger.entries > 0 && <span className="faint"> Ledger entries {inv.ledger.firstSequence}–{inv.ledger.lastSequence}.</span>}
              </p>
              <div className="table-wrap">
                <table>
                  <caption className="sr-only">{`Invoice ${inv.number} lines, ${monthLabel(month)}`}</caption>
                  <thead>
                    <tr>
                      <th scope="col">SKU</th>
                      <th scope="col">GPU type</th>
                      <th scope="col" className="num">GPU hours</th>
                      <th scope="col" className="num">Rate/hour</th>
                      <th scope="col" className="num">Amount</th>
                      <th scope="col" className="num">Jobs</th>
                    </tr>
                  </thead>
                  <tbody>
                    {inv.lines.map((l) => (
                      <tr key={`${l.sku}:${l.gpuType}`}>
                        <td className="mono">{l.sku}</td>
                        <td>{l.gpuType}</td>
                        <td className="num">{formatNumber(Math.round(l.gpuHours * 100) / 100)}</td>
                        <td className="num">{formatRate(l.rate, inv.currency)}</td>
                        <td className="num">{formatMoney(l.amount, inv.currency)}</td>
                        <td className="num">{formatNumber(l.jobs)}</td>
                      </tr>
                    ))}
                  </tbody>
                  <tfoot>
                    <tr>
                      <th scope="row" colSpan={4}>{invoiceTotalLabel(inv)}</th>
                      <td className="num"><b>{invoiceTotalDisplay(inv)}</b></td>
                      <td className="num">{formatNumber(inv.jobs)}</td>
                    </tr>
                  </tfoot>
                </table>
              </div>
              {inv.note && <p className="faint">{inv.note}</p>}
              {inv.networkLines && inv.networkLines.length > 0 && (
                <>
                  <h3 className="card-title">Network egress (estimate)</h3>
                  <div className="table-wrap">
                    <table>
                      <caption className="sr-only">{`Invoice ${inv.number} network egress lines, ${monthLabel(month)}`}</caption>
                      <thead>
                        <tr>
                          <th scope="col">Peer</th>
                          <th scope="col">Zone</th>
                          <th scope="col" className="num">Egress GB</th>
                          <th scope="col" className="num">Rate</th>
                          <th scope="col" className="num">Amount</th>
                        </tr>
                      </thead>
                      <tbody>
                        {inv.networkLines.map((l) => (
                          <tr key={`${l.peerClass}:${l.zoneClass}`}>
                            <td>{classLabel(l.peerClass)}</td>
                            <td>{classLabel(l.zoneClass)}</td>
                            <td className="num">{formatNumber(Math.round(l.egressGB * 1000) / 1000)}</td>
                            <td className="num">{networkRateDisplay(l)}</td>
                            <td className="num">{formatMoney(l.amount, l.currency || inv.currency)}</td>
                          </tr>
                        ))}
                      </tbody>
                      <tfoot>
                        <tr>
                          <th scope="row" colSpan={4}>Network subtotal (separate from the GPU subtotal)</th>
                          <td className="num"><b>{formatMoney(inv.networkSubtotal, inv.networkLines[0]?.currency || inv.currency)}</b></td>
                        </tr>
                      </tfoot>
                    </table>
                  </div>
                  {inv.networkNote && <p className="faint">{inv.networkNote}</p>}
                </>
              )}
              <div className="toolbar">
                {(['csv', 'json'] as const).map((f) => (
                  <button key={f} type="button" className="btn-secondary" onClick={() => download(inv, f)} disabled={busy !== null} aria-busy={busy === `${inv.number}:${f}`} aria-label={`Download ${f.toUpperCase()} for ${inv.number}`}>
                    {busy === `${inv.number}:${f}` ? 'Downloading…' : `Download ${f.toUpperCase()}`}
                  </button>
                ))}
                {actions.finalize && (
                  <button type="button" className="primary" onClick={() => setFinalizing(inv)} disabled={finalize.isPending}>
                    Finalize
                  </button>
                )}
                {actions.void && (
                  <button type="button" className="danger" onClick={() => setVoiding(inv)} disabled={voidInv.isPending}>
                    Void
                  </button>
                )}
              </div>
            </section>
            )
          })
        )}
      </div>
      {finalizing && (
        <ConfirmDialog
          title={`Finalize ${finalizing.tenant}, ${monthLabel(month)}?`}
          confirmLabel="Finalize invoice"
          tone="primary"
          busy={finalize.isPending}
          onCancel={() => setFinalizing(null)}
          onConfirm={() => finalize.mutate(finalizing)}
        >
          The lines are rebuilt from the billing ledger and frozen under the tenant&apos;s next invoice number. A finalized invoice cannot be edited or deleted, only voided.
        </ConfirmDialog>
      )}
      {voiding && (
        <ConfirmDialog
          title={`Void ${voiding.number}?`}
          confirmLabel="Void invoice"
          busy={voidInv.isPending}
          onCancel={() => {
            setVoiding(null)
            setReason('')
          }}
          onConfirm={() => {
            if (!reasonError) voidInv.mutate(voiding)
          }}
        >
          <p>The invoice is kept for audit with its number; the month can then be finalized again under a new number.</p>
          <label>
            Reason
            <textarea value={reason} onChange={(e) => setReason(e.target.value)} maxLength={500} rows={3} aria-invalid={!!reasonError} aria-describedby="void-reason-error" />
          </label>
          {reasonError && <p id="void-reason-error" className="faint">{reasonError}</p>}
        </ConfirmDialog>
      )}
    </>
  )
}
