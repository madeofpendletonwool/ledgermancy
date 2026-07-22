import type { ReactNode } from 'react'
import { Navigate, Route, Routes } from 'react-router-dom'
import { AppLayout } from './components/AppLayout'
import { Sigil } from './components/Brand'
import { Accounts } from './routes/Accounts'
import { Alerts } from './routes/Alerts'
import { Assistant } from './routes/Assistant'
import { Dashboard } from './routes/Dashboard'
import { Household } from './routes/Household'
import { Transactions } from './routes/Transactions'
import { Login } from './routes/Login'
import { NetWorth } from './routes/NetWorth'
import { Spending } from './routes/Spending'
import { Register } from './routes/Register'
import { Report } from './routes/Report'
import { Security } from './routes/Security'
import { useSession } from './lib/session'

export default function App() {
  return (
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
        <Route index element={<Dashboard />} />
        <Route path="/accounts" element={<Accounts />} />
        <Route path="/spending" element={<Spending />} />
        <Route path="/net-worth" element={<NetWorth />} />
        <Route path="/report" element={<Report />} />
        <Route path="/transactions" element={<Transactions />} />
        <Route path="/alerts" element={<Alerts />} />
        <Route path="/assistant" element={<Assistant />} />
        <Route path="/household" element={<Household />} />
        <Route path="/security" element={<Security />} />
      </Route>

      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
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
