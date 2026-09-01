import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom';
import { AuthProvider } from '../auth/AuthContext';
import { RequireAuth, RequirePermission } from '../auth/RouteGuards';
import { ErrorBoundary } from '../components/ErrorBoundary';
import { AppShell } from '../layout/AppShell';
import { AuditPage } from '../pages/admin/AuditPage';
import { AdminResourcePage, resourceConfigs } from '../pages/admin/AdminResourcePage';
import { RolesPage } from '../pages/admin/RolesPage';
import { SettingsPage } from '../pages/admin/SettingsPage';
import { DashboardPage } from '../pages/DashboardPage';
import { LoginPage } from '../pages/LoginPage';
import { ApiKeysPage } from '../pages/personal/ApiKeysPage';
import { ProfilePage } from '../pages/personal/ProfilePage';
import { ReleaseDetailPage } from '../pages/releases/ReleaseDetailPage';
import { ReleasesPage } from '../pages/releases/ReleasesPage';
import { NewReleasePage } from '../pages/releases/NewReleasePage';
import { AdvancedReleasePage } from '../pages/releases/AdvancedReleasePage';
import { SimpleDeployPage } from '../pages/simple/SimpleDeployPage';
import { GuideDetailPage } from '../pages/guides/GuideDetailPage';
import { GuidesPage } from '../pages/guides/GuidesPage';
import { SimpleRunDetailPage } from '../pages/simple/SimpleRunDetailPage';
import { SimpleRunsPage } from '../pages/simple/SimpleRunsPage';
import { ForbiddenPage, NotFoundPage } from '../pages/SystemPages';
import { UiModeProvider, useUiMode } from './UiModeContext';
import { VersionProvider } from './VersionContext';

export function App() {
  return (
    <ErrorBoundary>
      <BrowserRouter>
        <VersionProvider>
          <AuthProvider>
            <UiModeProvider>
            <Routes>
              <Route path="/login" element={<LoginPage />} />
              <Route element={<RequireAuth />}>
                <Route element={<AppShell />}>
                  <Route index element={<HomeRoute />} />
                  <Route path="simple" element={<RequirePermission permission="simple.deploy"><SimpleDeployPage /></RequirePermission>} />
                  <Route path="simple/runs" element={<RequirePermission permission="simple.read"><SimpleRunsPage /></RequirePermission>} />
                  <Route path="simple/runs/:id" element={<RequirePermission permission="simple.read"><SimpleRunDetailPage /></RequirePermission>} />
                  <Route path="releases" element={<RequirePermission permission="releases.read"><ReleasesPage /></RequirePermission>} />
                  <Route path="releases/new" element={<RequirePermission permission="releases.create"><NewReleasePage /></RequirePermission>} />
                  <Route path="releases/new/advanced" element={<RequirePermission permission="releases.create"><RequirePermission permission="applications.read"><RequirePermission permission="profiles.read"><AdvancedReleasePage /></RequirePermission></RequirePermission></RequirePermission>} />
                  <Route path="releases/:id" element={<RequirePermission permission="releases.read"><ReleaseDetailPage /></RequirePermission>} />
                  <Route path="personal/profile" element={<ProfilePage />} />
                  <Route path="personal/api-keys" element={<RequirePermission permission="keys.manage"><ApiKeysPage /></RequirePermission>} />
                  <Route path="guides" element={<RequirePermission permission="guides.read"><GuidesPage /></RequirePermission>} />
                  <Route path="guides/:id" element={<RequirePermission permission="guides.read"><GuideDetailPage /></RequirePermission>} />
                  <Route path="forbidden" element={<ForbiddenPage />} />

                  <Route path="admin/applications" element={<RequirePermission permission="applications.read"><AdminResourcePage config={resourceConfigs.applications} /></RequirePermission>} />
                  <Route path="admin/environments" element={<RequirePermission permission="applications.read"><AdminResourcePage config={resourceConfigs.environments} /></RequirePermission>} />
                  <Route path="admin/deployment-presets" element={<RequirePermission permission="admin.presets.read"><AdminResourcePage config={resourceConfigs.presets} /></RequirePermission>} />
                  <Route path="admin/deployment-profiles" element={<RequirePermission permission="profiles.read"><AdminResourcePage config={resourceConfigs.profiles} /></RequirePermission>} />
                  <Route path="admin/scripts" element={<RequirePermission permission="admin.scripts.read"><AdminResourcePage config={resourceConfigs.scripts} /></RequirePermission>} />
                  <Route path="admin/registries" element={<RequirePermission permission="admin.registries.read"><AdminResourcePage config={resourceConfigs.registries} /></RequirePermission>} />
                  <Route path="admin/target-credentials" element={<RequirePermission permission="admin.credentials.read"><AdminResourcePage config={resourceConfigs.targetCredentials} /></RequirePermission>} />
                  <Route path="admin/runners" element={<RequirePermission permission="admin.runners.read"><AdminResourcePage config={resourceConfigs.runners} /></RequirePermission>} />
                  <Route path="admin/users" element={<RequirePermission permission="admin.users.read"><AdminResourcePage config={resourceConfigs.users} /></RequirePermission>} />
                  <Route path="admin/roles" element={<RequirePermission permission="admin.rbac.read"><RolesPage /></RequirePermission>} />
                  <Route path="admin/audit" element={<RequirePermission permission="audit.read"><AuditPage /></RequirePermission>} />
                  <Route path="admin/settings/general" element={<RequirePermission permission="admin.settings.read"><SettingsPage section="general" /></RequirePermission>} />
                  <Route path="admin/settings/oidc" element={<RequirePermission permission="admin.settings.read"><SettingsPage section="oidc" /></RequirePermission>} />
                  <Route path="admin/settings/ai" element={<RequirePermission permission="admin.settings.read"><SettingsPage section="ai" /></RequirePermission>} />
                  <Route path="admin/settings/approval" element={<RequirePermission permission="admin.settings.read"><SettingsPage section="approval" /></RequirePermission>} />
                  <Route path="admin/settings/storage" element={<RequirePermission permission="admin.settings.read"><SettingsPage section="storage" /></RequirePermission>} />
                  <Route path="admin/settings/runner" element={<RequirePermission permission="admin.settings.read"><SettingsPage section="runner" /></RequirePermission>} />
                  <Route path="admin/guides" element={<RequirePermission permission="admin.guides.read"><AdminResourcePage config={resourceConfigs.guides} /></RequirePermission>} />
                  <Route path="admin/simple-targets" element={<RequirePermission permission="admin.simple.read"><AdminResourcePage config={resourceConfigs.simpleTargets} /></RequirePermission>} />
                  <Route path="admin/settings/simple" element={<RequirePermission permission="admin.simple.read"><SettingsPage section="simple" /></RequirePermission>} />
                  <Route path="admin/settings/network" element={<RequirePermission permission="admin.settings.read"><SettingsPage section="network" /></RequirePermission>} />
                  <Route path="*" element={<NotFoundPage />} />
                </Route>
              </Route>
            </Routes>
            </UiModeProvider>
          </AuthProvider>
        </VersionProvider>
      </BrowserRouter>
    </ErrorBoundary>
  );
}

// HomeRoute sends each user to the landing screen for their active mode, so
// simple-mode users never see the release dashboard they cannot act on.
function HomeRoute() {
  const { mode, loading } = useUiMode();
  if (loading) return null;
  if (mode === 'simple') return <Navigate to="/simple" replace />;
  return (
    <RequirePermission permission="releases.read">
      <DashboardPage />
    </RequirePermission>
  );
}
