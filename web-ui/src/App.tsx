import { lazy, Suspense, useEffect } from 'react'
import { BrowserRouter as Router, Routes, Route, Navigate, Link, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import axios from 'axios'
import { Toaster } from 'react-hot-toast'

import ErrorBoundary from './components/ErrorBoundary'
import AuthProvider from './components/AuthProvider'
import Layout from './components/Layout'
import PageHero from './components/PageHero'
import { Skeleton } from './components/StateViews'
import { useAuth } from './lib/auth'
import { TENANT_HOME } from './lib/roles'
import { useIsAdmin } from './lib/useRole'
import { setUnauthorizedHandler } from './lib/authEvents'
import { notify } from './lib/notify'
import { useDocumentTitle } from './hooks/useDocumentTitle'
import Login from './pages/Login'
import AuthCallback from './pages/AuthCallback'

// Pages load on demand so the first paint (login, dashboard) stays small.
const Dashboard = lazy(() => import('./pages/Dashboard'))
const Jobs = lazy(() => import('./pages/Jobs'))
const JobDetails = lazy(() => import('./pages/JobDetails'))
const SubmitJob = lazy(() => import('./pages/SubmitJob'))
const Quotas = lazy(() => import('./pages/Quotas'))
const Nodes = lazy(() => import('./pages/Nodes'))
const Costs = lazy(() => import('./pages/Costs'))
const NetworkOverview = lazy(() => import('./pages/NetworkOverview'))
const NetworkFlows = lazy(() => import('./pages/NetworkFlows'))
const NetworkPolicies = lazy(() => import('./pages/NetworkPolicies'))
const NetworkCost = lazy(() => import('./pages/NetworkCost'))
const SecurityOverview = lazy(() => import('./pages/SecurityOverview'))
const Sovereign = lazy(() => import('./pages/Sovereign'))
const GpuCommunication = lazy(() => import('./pages/GpuCommunication'))
const Workspaces = lazy(() => import('./pages/Workspaces'))
const ModelRegistry = lazy(() => import('./pages/ModelRegistry'))
const ModelFactory = lazy(() => import('./pages/ModelFactory'))
const Datasets = lazy(() => import('./pages/Datasets'))
const DataCatalog = lazy(() => import('./pages/DataCatalog'))
const Copilot = lazy(() => import('./pages/Copilot'))
const Intelligence = lazy(() => import('./pages/Intelligence'))
const Audit = lazy(() => import('./pages/Audit'))
const Lineage = lazy(() => import('./pages/Lineage'))
const LlmGateway = lazy(() => import('./pages/LlmGateway'))
const Playground = lazy(() => import('./pages/Playground'))
const VectorIndexes = lazy(() => import('./pages/VectorIndexes'))
const Agents = lazy(() => import('./pages/Agents'))
const InferenceServices = lazy(() => import('./pages/InferenceServices'))
const Workflows = lazy(() => import('./pages/Workflows'))
const AutoTuner = lazy(() => import('./pages/AutoTuner'))
const Catalog = lazy(() => import('./pages/Catalog'))
const Usage = lazy(() => import('./pages/Usage'))
const Reservations = lazy(() => import('./pages/Reservations'))
const Budgets = lazy(() => import('./pages/Budgets'))
const Experiments = lazy(() => import('./pages/Experiments'))
const Invoices = lazy(() => import('./pages/Invoices'))
const Tenants = lazy(() => import('./pages/Tenants'))

const queryClient = new QueryClient({
  queryCache: new QueryCache({
    // A background refresh failing while we still show data: say so once (the pill also turns "Stale").
    onError: (error, query) => {
      if (query.state.data !== undefined) notify.error('Refresh failed', error)
    },
  }),
  defaultOptions: {
    queries: {
      refetchOnWindowFocus: false,
      refetchIntervalInBackground: false,
      staleTime: 30000,
      // Don't retry client errors (404/422/429): they will not fix themselves.
      retry: (failureCount, error) => {
        if (axios.isAxiosError(error) && error.response && error.response.status < 500) return false
        return failureCount < 1
      },
    },
  },
})

/** Sends a signed-out or expired session to /login, remembering where the user was. */
function SessionGuard() {
  const navigate = useNavigate()
  const { logout } = useAuth()
  useEffect(() => {
    setUnauthorizedHandler(({ reason }) => {
      const { pathname, search, hash } = window.location
      logout()
      navigate('/login', { replace: true, state: { from: { pathname, search, hash }, reason } })
    })
    return () => setUnauthorizedHandler(undefined)
  }, [logout, navigate])
  return null
}

/** Auth gate + shell for every signed-in page. */
function ProtectedLayout() {
  const { isAuthenticated, isLoading } = useAuth()
  const location = useLocation()

  if (isLoading) {
    return (
      <div className="login-shell">
        <div className="spinner" role="status" aria-label="Loading" />
      </div>
    )
  }
  if (!isAuthenticated) {
    return <Navigate to="/login" state={{ from: location }} replace />
  }
  return (
    <Layout>
      <ErrorBoundary key={location.pathname}>
        <Suspense fallback={<Skeleton rows={4} />}>
          <Outlet />
        </Suspense>
      </ErrorBoundary>
    </Layout>
  )
}

/** Routes only admins may open; a tenant user is sent to the catalog instead. */
function AdminOnly() {
  const admin = useIsAdmin()
  return admin ? <Outlet /> : <Navigate to={TENANT_HOME} replace />
}

function NotFound() {
  useDocumentTitle('Page not found')
  const admin = useIsAdmin()
  return (
    <>
      <PageHero eyebrow="404" title="Page not found." lede="That page doesn't exist. Try one of these instead." />
      <div className="toolbar">
        {[
          ['/dashboard', 'Dashboard'],
          ['/jobs', 'Jobs'],
          ['/workspaces', 'Workspaces'],
          ...(admin ? [['/nodes', 'Nodes'], ['/quotas', 'Quotas']] : [['/catalog', 'Catalog'], ['/usage', 'Usage']]),
        ].map(([to, label]) => (
          <Link key={to} to={to} className="buttonlike btn-secondary">
            {label}
          </Link>
        ))}
      </div>
    </>
  )
}

function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <Router>
        <AuthProvider>
          <SessionGuard />
          <Routes>
            <Route path="/login" element={<Login />} />
            <Route path="/auth/callback" element={<AuthCallback />} />
            <Route element={<ProtectedLayout />}>
              <Route index element={<Navigate to="/dashboard" replace />} />
              <Route path="/dashboard" element={<Dashboard />} />
              <Route path="/intelligence" element={<Intelligence />} />
              <Route path="/jobs" element={<Jobs />} />
              <Route path="/jobs/new" element={<SubmitJob />} />
              <Route path="/jobs/:name" element={<JobDetails />} />
              <Route element={<AdminOnly />}>
                <Route path="/sovereign" element={<Sovereign />} />
                <Route path="/quotas" element={<Quotas />} />
                <Route path="/nodes" element={<Nodes />} />
                <Route path="/network" element={<NetworkOverview />} />
                <Route path="/network/flows" element={<NetworkFlows />} />
                <Route path="/network/policies" element={<NetworkPolicies />} />
                <Route path="/network/costs" element={<NetworkCost />} />
                <Route path="/security" element={<SecurityOverview />} />
                <Route path="/audit" element={<Audit />} />
                <Route path="/copilot" element={<Copilot />} />
                <Route path="/data-catalog" element={<DataCatalog />} />
                <Route path="/gpu" element={<GpuCommunication />} />
                <Route path="/gpu/communication" element={<GpuCommunication />} />
                <Route path="/costs" element={<Costs />} />
              </Route>
              <Route path="/catalog" element={<Catalog />} />
              <Route path="/usage" element={<Usage />} />
              <Route path="/reservations" element={<Reservations />} />
              <Route path="/budgets" element={<Budgets />} />
              <Route path="/invoices" element={<Invoices />} />
              <Route path="/tenants" element={<Tenants />} />
              <Route path="/workspaces" element={<Workspaces />} />
              <Route path="/models" element={<ModelRegistry />} />
              <Route path="/model-factory" element={<ModelFactory />} />
              <Route path="/datasets" element={<Datasets />} />
              <Route path="/lineage" element={<Lineage />} />
              <Route path="/llm" element={<LlmGateway />} />
              <Route path="/playground" element={<Playground />} />
              <Route path="/rag" element={<VectorIndexes />} />
              <Route path="/agents" element={<Agents />} />
              <Route path="/inference" element={<InferenceServices />} />
              <Route path="/workflows" element={<Workflows />} />
              <Route path="/experiments" element={<Experiments />} />
              <Route path="/tuner" element={<AutoTuner />} />
              <Route path="*" element={<NotFound />} />
            </Route>
          </Routes>
        </AuthProvider>
      </Router>
      <Toaster position="top-right" toastOptions={{ style: { background: 'var(--bg-elevated)', color: 'var(--text-primary)', border: '1px solid var(--hairline-1)' } }} />
    </QueryClientProvider>
  )
}

export default App
