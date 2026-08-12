import { lazy, Suspense, type ReactNode } from 'react'
import { Navigate, Route, Routes } from 'react-router-dom'
import { AppLayout } from './components/AppLayout'
import { Sigil } from './components/Brand'
import { ServiceWorkerHost } from './components/PwaPrompts'
import { isAdult } from './lib/api'
import { useSession } from './lib/session'

/**
 * Every route is a separate chunk (MAD-65).
 *
 * These were static imports, which meant one bundle carrying all 22 screens —
 * `d3-sankey`, the whole chart set, `react-markdown` — before first paint, for
 * a session that opens two of them. `lazy()` moves each screen behind a dynamic
 * `import()`, so the entry chunk is the shell and the router pulls a screen when
 * it is actually navigated to.
 *
 * The routes export named components rather than defaults, hence the `.then()`
 * on each: `lazy` wants a module whose `default` is the component. Written out
 * per route rather than hidden behind a helper so that each line stays a plain,
 * greppable "this path, that file".
 *
 * Two things to keep true when editing:
 *   - Nothing outside this file may import from `./routes/*`. A single static
 *     import anywhere else pulls that screen straight back into the entry chunk
 *     and the dynamic import here becomes dead weight.
 *   - Every screen still needs to be reachable offline. The service worker
 *     precaches `**\/*.js`, so new chunks are covered automatically — see
 *     `injectManifest.globPatterns` in vite.config.ts before narrowing that.
 */
const Accounts = lazy(() => import('./routes/Accounts').then((m) => ({ default: m.Accounts })))
const Advisor = lazy(() => import('./routes/Advisor').then((m) => ({ default: m.Advisor })))
const Alerts = lazy(() => import('./routes/Alerts').then((m) => ({ default: m.Alerts })))
const Budgets = lazy(() => import('./routes/Budgets').then((m) => ({ default: m.Budgets })))
const Categories = lazy(() =>
  import('./routes/Categories').then((m) => ({ default: m.Categories })),
)
const CategoryDetail = lazy(() =>
  import('./routes/CategoryDetail').then((m) => ({ default: m.CategoryDetail })),
)
const Dashboard = lazy(() => import('./routes/Dashboard').then((m) => ({ default: m.Dashboard })))
const Digest = lazy(() => import('./routes/Digest').then((m) => ({ default: m.Digest })))
const Documents = lazy(() => import('./routes/Documents').then((m) => ({ default: m.Documents })))
const Goals = lazy(() => import('./routes/Goals').then((m) => ({ default: m.Goals })))
const Insights = lazy(() => import('./routes/Insights').then((m) => ({ default: m.Insights })))
const Investments = lazy(() =>
  import('./routes/Investments').then((m) => ({ default: m.Investments })),
)
const Login = lazy(() => import('./routes/Login').then((m) => ({ default: m.Login })))
const MerchantDetail = lazy(() =>
  import('./routes/MerchantDetail').then((m) => ({ default: m.MerchantDetail })),
)
const Merchants = lazy(() => import('./routes/Merchants').then((m) => ({ default: m.Merchants })))
const MyMoney = lazy(() => import('./routes/MyMoney').then((m) => ({ default: m.MyMoney })))
const NetWorth = lazy(() => import('./routes/NetWorth').then((m) => ({ default: m.NetWorth })))
const Paystubs = lazy(() => import('./routes/Paystubs').then((m) => ({ default: m.Paystubs })))
const PiggyBanks = lazy(() =>
  import('./routes/PiggyBanks').then((m) => ({ default: m.PiggyBanks })),
)
const Register = lazy(() => import('./routes/Register').then((m) => ({ default: m.Register })))
const Reminders = lazy(() => import('./routes/Reminders').then((m) => ({ default: m.Reminders })))
const Report = lazy(() => import('./routes/Report').then((m) => ({ default: m.Report })))
const Retirement = lazy(() =>
  import('./routes/Retirement').then((m) => ({ default: m.Retirement })),
)
const Schedule = lazy(() => import('./routes/Schedule').then((m) => ({ default: m.Schedule })))
const Settings = lazy(() => import('./routes/Settings').then((m) => ({ default: m.Settings })))
const Shared = lazy(() => import('./routes/Shared').then((m) => ({ default: m.Shared })))
const Spending = lazy(() => import('./routes/Spending').then((m) => ({ default: m.Spending })))
const Transactions = lazy(() =>
  import('./routes/Transactions').then((m) => ({ default: m.Transactions })),
)

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
          {/* The stored digest history. Also the ntfy deep link target, so a
              push always lands on the kept copy rather than a feed that has
              moved on since. */}
          <Route path="/digest" element={<Digest />} />
          <Route path="/accounts" element={<Accounts />} />
          <Route path="/spending" element={<Spending />} />
          <Route path="/budgets" element={<Budgets />} />
          <Route path="/schedule" element={<Schedule />} />
          <Route path="/reminders" element={<Reminders />} />
          <Route path="/goals" element={<Goals />} />
          <Route path="/piggy-banks" element={<PiggyBanks />} />
          <Route path="/shared" element={<Shared />} />
          <Route path="/net-worth" element={<NetWorth />} />
          <Route path="/investments" element={<Investments />} />
          <Route path="/retirement" element={<Retirement />} />
          <Route path="/report" element={<Report />} />
          <Route path="/transactions" element={<Transactions />} />
          <Route path="/categories" element={<Categories />} />
          {/* Drill-down target; reached from any category name, not the nav. A
              category id is a UUID, so unlike a merchant key it is safe in the
              path. Registered after /categories so the literal wins. */}
          <Route path="/categories/:categoryId" element={<CategoryDetail />} />
          <Route path="/merchants" element={<Merchants />} />
          {/* Drill-down target; reached from any merchant name, not the nav.
              The merchant travels as ?key= rather than a path segment because a
              raw descriptor can contain a slash. */}
          <Route path="/merchants/detail" element={<MerchantDetail />} />
          <Route path="/documents" element={<Documents />} />
          <Route path="/paystubs" element={<Paystubs />} />
          <Route path="/alerts" element={<Alerts />} />
          <Route path="/advisor" element={<Advisor />} />
          {/* The Assistant became the Advisor (doc 31). The old path is kept so
              existing bookmarks and any saved link keep working. */}
          <Route path="/assistant" element={<Navigate to="/advisor" replace />} />
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

/**
 * Keeps a signed-in user away from the login and register screens.
 *
 * Also the Suspense boundary for the two public screens, which live outside
 * `AppLayout` and so are not covered by the one around the outlet. The fallback
 * is the same `<Loading/>` already shown while the session resolves, so a cold
 * load of /login is one continuous sigil rather than a spinner that blinks out
 * and back as the chunk lands.
 */
function PublicOnly({ children }: { children: ReactNode }) {
  const { data: user, isPending } = useSession()
  if (isPending) return <Loading />
  if (user) return <Navigate to="/" replace />
  return <Suspense fallback={<Loading />}>{children}</Suspense>
}

function Loading() {
  return (
    <div className="flex min-h-screen items-center justify-center">
      <Sigil className="h-12 w-12 animate-pulse" />
      <span className="sr-only">Loading</span>
    </div>
  )
}
