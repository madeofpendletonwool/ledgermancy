import type { ReactNode } from 'react'
import { Navigate, Route, Routes } from 'react-router-dom'
import { AppLayout } from './components/AppLayout'
import { Sigil } from './components/Brand'
import { ServiceWorkerHost } from './components/PwaPrompts'
import { Accounts } from './routes/Accounts'
import { Alerts } from './routes/Alerts'
import { Assistant } from './routes/Assistant'
import { Budgets } from './routes/Budgets'
import { Categories } from './routes/Categories'
import { MerchantDetail } from './routes/MerchantDetail'
import { Merchants } from './routes/Merchants'
import { Dashboard } from './routes/Dashboard'
import { Documents } from './routes/Documents'
import { Goals } from './routes/Goals'
import { Insights } from './routes/Insights'
import { Investments } from './routes/Investments'
import { Transactions } from './routes/Transactions'
import { Login } from './routes/Login'
import { NetWorth } from './routes/NetWorth'
import { Spending } from './routes/Spending'
import { Register } from './routes/Register'
import { Report } from './routes/Report'
import { Retirement } from './routes/Retirement'
import { Schedule } from './routes/Schedule'
import { Settings } from './routes/Settings'
import { MyMoney } from './routes/MyMoney'
import { Shared } from './routes/Shared'
import { isAdult } from './lib/api'
import { useSession } from './lib/session'

export default function App() {
  return (
    <>
      {/* Outside the router: the worker should register and offer updates on
          every route, signed in or not. */}
      <ServiceWorkerHost />
      <Routes>
        <Route
          path="/login"
          element={
            <PublicOnly>
              <Login />
            </PublicOnly>
          }
        />
        <Route
          path="/register"
          element={
            <PublicOnly>
              <Register />
            </PublicOnly>
          }
        />

        <Route
          element={
            <RequireAuth>
              <AppLayout />
            </RequireAuth>
          }
        >
          {/* A child's home is their own money, not the household dashboard.
              This is presentation only — every household route below is
              guarded server-side, so a child who types a URL gets a 403 from
              the API regardless of what the router renders. */}
          <Route index element={<Home />} />
          <Route path="/insights" element={<Insights />} />
          <Route path="/accounts" element={<Accounts />} />
          <Route path="/spending" element={<Spending />} />
          <Route path="/budgets" element={<Budgets />} />
          <Route path="/schedule" element={<Schedule />} />
          <Route path="/goals" element={<Goals />} />
          <Route path="/shared" element={<Shared />} />
          <Route path="/net-worth" element={<NetWorth />} />
          <Route path="/investments" element={<Investments />} />
          <Route path="/retirement" element={<Retirement />} />
          <Route path="/report" element={<Report />} />
          <Route path="/transactions" element={<Transactions />} />
          <Route path="/categories" element={<Categories />} />
          <Route path="/merchants" element={<Merchants />} />
          {/* Drill-down target; reached from any merchant name, not the nav.
              The merchant travels as ?key= rather than a path segment because a
              raw descriptor can contain a slash. */}
          <Route path="/merchants/detail" element={<MerchantDetail />} />
          <Route path="/documents" element={<Documents />} />
          <Route path="/alerts" element={<Alerts />} />
          <Route path="/assistant" element={<Assistant />} />
          <Route path="/settings" element={<Settings />} />
          {/* Old paths preserved so existing bookmarks keep working; Household and
              Security now live as tabs inside Settings. */}
          <Route
            path="/household"
            element={<Navigate to="/settings?tab=household" replace />}
          />
          <Route path="/security" element={<Navigate to="/settings" replace />} />
        </Route>

        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </>
  )
}

/**
 * The landing page, by role. A child gets their own money; everyone else gets
 * the household dashboard.
 */
function Home() {
  const { data: user } = useSession()
  return isAdult(user) ? <Dashboard /> : <MyMoney />
}

/** Blocks a route until the session resolves, then redirects if signed out. */
function RequireAuth({ children }: { children: ReactNode }) {
  const { data: user, isPending } = useSession()
  if (isPending) return <Loading />
  if (!user) return <Navigate to="/login" replace />
  return <>{children}</>
}

/** Keeps a signed-in user away from the login and register screens. */
function PublicOnly({ children }: { children: ReactNode }) {
  const { data: user, isPending } = useSession()
  if (isPending) return <Loading />
  if (user) return <Navigate to="/" replace />
  return <>{children}</>
}

function Loading() {
  return (
    <div className="flex min-h-screen items-center justify-center">
      <Sigil className="h-12 w-12 animate-pulse" />
      <span className="sr-only">Loading</span>
    </div>
  )
}
