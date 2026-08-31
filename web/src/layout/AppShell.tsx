import AccountCircleRoundedIcon from '@mui/icons-material/AccountCircleRounded';
import AdminPanelSettingsRoundedIcon from '@mui/icons-material/AdminPanelSettingsRounded';
import AppsRoundedIcon from '@mui/icons-material/AppsRounded';
import ApprovalRoundedIcon from '@mui/icons-material/ApprovalRounded';
import AutoAwesomeRoundedIcon from '@mui/icons-material/AutoAwesomeRounded';
import BadgeRoundedIcon from '@mui/icons-material/BadgeRounded';
import ChevronLeftRoundedIcon from '@mui/icons-material/ChevronLeftRounded';
import ChevronRightRoundedIcon from '@mui/icons-material/ChevronRightRounded';
import CloudQueueRoundedIcon from '@mui/icons-material/CloudQueueRounded';
import DashboardRoundedIcon from '@mui/icons-material/DashboardRounded';
import DnsRoundedIcon from '@mui/icons-material/DnsRounded';
import ExpandLessRoundedIcon from '@mui/icons-material/ExpandLessRounded';
import ExpandMoreRoundedIcon from '@mui/icons-material/ExpandMoreRounded';
import HistoryRoundedIcon from '@mui/icons-material/HistoryRounded';
import KeyRoundedIcon from '@mui/icons-material/KeyRounded';
import LanRoundedIcon from '@mui/icons-material/LanRounded';
import LogoutRoundedIcon from '@mui/icons-material/LogoutRounded';
import MenuRoundedIcon from '@mui/icons-material/MenuRounded';
import MemoryRoundedIcon from '@mui/icons-material/MemoryRounded';
import PersonRoundedIcon from '@mui/icons-material/PersonRounded';
import PlaylistAddRoundedIcon from '@mui/icons-material/PlaylistAddRounded';
import RocketLaunchRoundedIcon from '@mui/icons-material/RocketLaunchRounded';
import SettingsRoundedIcon from '@mui/icons-material/SettingsRounded';
import StorageRoundedIcon from '@mui/icons-material/StorageRounded';
import TerminalRoundedIcon from '@mui/icons-material/TerminalRounded';
import TuneRoundedIcon from '@mui/icons-material/TuneRounded';
import {
  AppBar,
  Avatar,
  Box,
  Collapse,
  Divider,
  Drawer,
  IconButton,
  List,
  ListItemButton,
  ListItemIcon,
  ListItemText,
  Menu,
  MenuItem,
  Stack,
  Toolbar,
  Tooltip,
  Typography,
  useMediaQuery,
} from '@mui/material';
import { alpha, useTheme } from '@mui/material/styles';
import { useEffect, useMemo, useState, type ElementType, type MouseEvent } from 'react';
import { Link as RouterLink, Outlet, useLocation, useNavigate } from 'react-router-dom';
import { useUiMode } from '../app/UiModeContext';
import { useVersion } from '../app/VersionContext';
import { useAuth } from '../auth/AuthContext';
import { drawerWidth } from '../theme';

interface NavItem {
  label: string;
  path: string;
  icon: ElementType;
  permission?: string;
}

interface NavSection {
  id: 'workspace' | 'admin' | 'personal';
  label: string;
  items: NavItem[];
  adminOnly?: boolean;
}

// Simple mode deliberately exposes only the two screens it needs, plus the
// simple-mode administration a full-mode operator would otherwise have to go
// looking for. Everything else stays hidden until the user switches modes.
const simpleSections: NavSection[] = [
  {
    id: 'workspace',
    label: '배포',
    items: [
      { label: '배포', path: '/simple', icon: PlaylistAddRoundedIcon, permission: 'simple.deploy' },
      { label: '실행 기록', path: '/simple/runs', icon: HistoryRoundedIcon, permission: 'simple.read' },
    ],
  },
  {
    id: 'admin',
    label: '관리',
    adminOnly: true,
    items: [
      { label: '심플 대상', path: '/admin/simple-targets', icon: DnsRoundedIcon, permission: 'admin.simple.read' },
      { label: '심플 모드 설정', path: '/admin/settings/simple', icon: TuneRoundedIcon, permission: 'admin.simple.read' },
      { label: '관리자 접근 IP', path: '/admin/settings/network', icon: LanRoundedIcon, permission: 'admin.settings.read' },
      { label: '사용자', path: '/admin/users', icon: PersonRoundedIcon, permission: 'admin.users.read' },
      { label: 'Keycloak OIDC', path: '/admin/settings/oidc', icon: LanRoundedIcon, permission: 'admin.settings.read' },
      { label: '감사 로그', path: '/admin/audit', icon: HistoryRoundedIcon, permission: 'audit.read' },
    ],
  },
  {
    id: 'personal',
    label: '개인화',
    items: [{ label: '내 프로필', path: '/personal/profile', icon: BadgeRoundedIcon }],
  },
];

const sections: NavSection[] = [
  {
    id: 'workspace',
    label: '릴리즈 작업공간',
    items: [
      { label: '대시보드', path: '/', icon: DashboardRoundedIcon, permission: 'releases.read' },
      { label: '릴리즈', path: '/releases', icon: RocketLaunchRoundedIcon, permission: 'releases.read' },
      { label: '새 버전 배포', path: '/releases/new', icon: PlaylistAddRoundedIcon, permission: 'releases.create' },
      { label: '심플 배포', path: '/simple', icon: DnsRoundedIcon, permission: 'simple.deploy' },
    ],
  },
  {
    id: 'admin',
    label: '서비스 관리',
    adminOnly: true,
    items: [
      { label: '애플리케이션', path: '/admin/applications', icon: AppsRoundedIcon, permission: 'applications.read' },
      { label: '환경', path: '/admin/environments', icon: CloudQueueRoundedIcon, permission: 'applications.read' },
      { label: '배포 프리셋', path: '/admin/deployment-presets', icon: TuneRoundedIcon, permission: 'admin.presets.read' },
      { label: '배포 프로필', path: '/admin/deployment-profiles', icon: TuneRoundedIcon, permission: 'profiles.read' },
      { label: '스크립트', path: '/admin/scripts', icon: TerminalRoundedIcon, permission: 'admin.scripts.read' },
      { label: 'Harbor Registry', path: '/admin/registries', icon: StorageRoundedIcon, permission: 'admin.registries.read' },
      { label: '배포 대상 Credential', path: '/admin/target-credentials', icon: KeyRoundedIcon, permission: 'admin.credentials.read' },
      { label: 'Runner', path: '/admin/runners', icon: MemoryRoundedIcon, permission: 'admin.runners.read' },
      { label: 'Runner 설정', path: '/admin/settings/runner', icon: TuneRoundedIcon, permission: 'admin.settings.read' },
      { label: '승인 정책', path: '/admin/settings/approval', icon: ApprovalRoundedIcon, permission: 'admin.settings.read' },
      { label: 'AI 설정', path: '/admin/settings/ai', icon: AutoAwesomeRoundedIcon, permission: 'admin.settings.read' },
      { label: 'Keycloak OIDC', path: '/admin/settings/oidc', icon: LanRoundedIcon, permission: 'admin.settings.read' },
      { label: '일반 설정', path: '/admin/settings/general', icon: SettingsRoundedIcon, permission: 'admin.settings.read' },
      { label: '아티팩트 스토리지', path: '/admin/settings/storage', icon: StorageRoundedIcon, permission: 'admin.settings.read' },
      { label: '사용자', path: '/admin/users', icon: PersonRoundedIcon, permission: 'admin.users.read' },
      { label: '역할 및 권한', path: '/admin/roles', icon: AdminPanelSettingsRoundedIcon, permission: 'admin.rbac.read' },
      { label: '감사 로그', path: '/admin/audit', icon: HistoryRoundedIcon, permission: 'audit.read' },
      { label: '심플 대상', path: '/admin/simple-targets', icon: DnsRoundedIcon, permission: 'admin.simple.read' },
      { label: '심플 모드 설정', path: '/admin/settings/simple', icon: TuneRoundedIcon, permission: 'admin.simple.read' },
      { label: '관리자 접근 IP', path: '/admin/settings/network', icon: LanRoundedIcon, permission: 'admin.settings.read' },
    ],
  },
  {
    id: 'personal',
    label: '개인화',
    items: [
      { label: '내 프로필', path: '/personal/profile', icon: BadgeRoundedIcon },
      { label: '내 API 키', path: '/personal/api-keys', icon: KeyRoundedIcon, permission: 'keys.manage' },
    ],
  },
];

const EXPANDED_KEY = 'releasedock.nav.expanded';

function initialExpanded(): Record<NavSection['id'], boolean> {
  try {
    const stored = JSON.parse(window.localStorage.getItem(EXPANDED_KEY) ?? '{}') as Partial<Record<NavSection['id'], boolean>>;
    return { workspace: stored.workspace ?? true, admin: stored.admin ?? true, personal: stored.personal ?? true };
  } catch {
    return { workspace: true, admin: true, personal: true };
  }
}

function Logo() {
  return (
    <Stack direction="row" alignItems="center" spacing={1.3} sx={{ minWidth: 0 }}>
      <Box
        aria-hidden="true"
        sx={{
          width: 38,
          height: 38,
          borderRadius: 2.25,
          display: 'grid',
          placeItems: 'center',
          color: '#06111f',
          background: 'linear-gradient(145deg, #78b7ff, #45dbb4)',
          boxShadow: '0 8px 22px rgba(72, 160, 235, .28)',
        }}
      >
        <DnsRoundedIcon fontSize="small" />
      </Box>
      <Box sx={{ minWidth: 0 }}>
        <Typography fontWeight={800} letterSpacing="-.02em" noWrap>
          ReleaseDock
        </Typography>
        <Typography variant="caption" color="text.secondary" noWrap>
          Deployment Gateway
        </Typography>
      </Box>
    </Stack>
  );
}

function Navigation({ onNavigate }: { onNavigate: () => void }) {
  const { pathname } = useLocation();
  const { hasPermission } = useAuth();
  const { mode } = useUiMode();
  const [expanded, setExpanded] = useState(initialExpanded);
  const activeSections = mode === 'simple' ? simpleSections : sections;

  useEffect(() => {
    window.localStorage.setItem(EXPANDED_KEY, JSON.stringify(expanded));
  }, [expanded]);

  useEffect(() => {
    const matchingSection = activeSections.find((section) => section.items.some((item) => pathname === item.path || (item.path !== '/' && pathname.startsWith(`${item.path}/`))));
    if (matchingSection) setExpanded((current) => ({ ...current, [matchingSection.id]: true }));
  }, [pathname, activeSections]);

  return (
    <Box component="nav" aria-label="주 메뉴" sx={{ flex: 1, minHeight: 0, overflowY: 'auto', px: 1.25, py: 1.5 }}>
      {activeSections.map((section) => ({ ...section, items: section.items.filter((item) => !item.permission || hasPermission(item.permission)) })).filter((section) => section.items.length > 0).map((section) => (
        <Box key={section.id} sx={{ mb: 1 }}>
          <ListItemButton
            onClick={() => setExpanded((current) => ({ ...current, [section.id]: !current[section.id] }))}
            aria-expanded={expanded[section.id]}
            aria-controls={`nav-${section.id}`}
            sx={{ borderRadius: 2, minHeight: 42, color: 'text.secondary' }}
          >
            <ListItemText primary={section.label} primaryTypographyProps={{ variant: 'caption', fontWeight: 800, letterSpacing: '.07em' }} />
            {expanded[section.id] ? <ExpandLessRoundedIcon fontSize="small" /> : <ExpandMoreRoundedIcon fontSize="small" />}
          </ListItemButton>
          <Collapse in={expanded[section.id]} timeout="auto" unmountOnExit={false} id={`nav-${section.id}`}>
            <List disablePadding>
              {section.items.map((item) => {
                const selected = pathname === item.path || (item.path !== '/' && pathname.startsWith(`${item.path}/`));
                const Icon = item.icon;
                return (
                  <ListItemButton
                    component={RouterLink}
                    to={item.path}
                    key={item.path}
                    selected={selected}
                    onClick={onNavigate}
                    aria-current={selected ? 'page' : undefined}
                    sx={{
                      borderRadius: 2,
                      minHeight: 46,
                      my: 0.35,
                      pl: 1.5,
                      '&.Mui-selected': {
                        color: 'primary.light',
                        backgroundColor: alpha('#68a9ff', 0.14),
                        '&:hover': { backgroundColor: alpha('#68a9ff', 0.2) },
                        '&::before': {
                          content: '""',
                          position: 'absolute',
                          left: 0,
                          top: 10,
                          bottom: 10,
                          width: 3,
                          borderRadius: 4,
                          backgroundColor: 'primary.main',
                        },
                      },
                    }}
                  >
                    <ListItemIcon sx={{ minWidth: 39, color: 'inherit' }}>
                      <Icon fontSize="small" />
                    </ListItemIcon>
                    <ListItemText primary={item.label} primaryTypographyProps={{ fontSize: '0.9375rem', fontWeight: selected ? 700 : 550 }} />
                  </ListItemButton>
                );
              })}
            </List>
          </Collapse>
        </Box>
      ))}
    </Box>
  );
}

export function AppShell() {
  const theme = useTheme();
  const desktop = useMediaQuery(theme.breakpoints.up('lg'));
  const [mobileOpen, setMobileOpen] = useState(false);
  const [profileAnchor, setProfileAnchor] = useState<HTMLElement | null>(null);
  const { user, logout, hasPermission } = useAuth();
  const { mode, canUseSimple, canUseFull, setMode } = useUiMode();
  const version = useVersion();
  const navigate = useNavigate();

  const initials = useMemo(() => (user?.displayName || user?.username || 'U').slice(0, 2).toUpperCase(), [user]);
  const closeProfile = () => setProfileAnchor(null);
  const handleProfileMenu = (event: MouseEvent<HTMLElement>) => setProfileAnchor(event.currentTarget);
  const handleLogout = async () => {
    closeProfile();
    try {
      await logout();
    } catch {
      // Local session state is cleared even if the server is temporarily unavailable.
    } finally {
      navigate('/login', { replace: true });
    }
  };

  const drawer = (
    <Box sx={{ height: '100%', display: 'flex', flexDirection: 'column', bgcolor: '#0b1220' }}>
      <Toolbar sx={{ px: 2.25, minHeight: '72px !important' }}>
        <Logo />
        {!desktop && (
          <IconButton aria-label="메뉴 닫기" onClick={() => setMobileOpen(false)} sx={{ ml: 'auto' }}>
            <ChevronLeftRoundedIcon />
          </IconButton>
        )}
      </Toolbar>
      <Divider />
      <Navigation onNavigate={() => setMobileOpen(false)} />
      <Divider />
      <Box sx={{ p: 1.5 }}>
        <Typography variant="caption" color="text.secondary" sx={{ px: 1 }}>
          안전한 릴리즈 오케스트레이션
        </Typography>
      </Box>
    </Box>
  );

  return (
    <Box sx={{ display: 'flex', minHeight: '100vh' }}>
      <a className="skip-link" href="#main-content">본문으로 건너뛰기</a>
      <AppBar
        position="fixed"
        elevation={0}
        sx={{
          width: { lg: `calc(100% - ${drawerWidth}px)` },
          ml: { lg: `${drawerWidth}px` },
          bgcolor: alpha('#080e19', 0.82),
          backdropFilter: 'blur(14px)',
          borderBottom: `1px solid ${theme.palette.divider}`,
        }}
      >
        <Toolbar sx={{ minHeight: '72px !important', gap: 1.5 }}>
          <IconButton aria-label="주 메뉴 열기" edge="start" onClick={() => setMobileOpen(true)} sx={{ display: { lg: 'none' } }}>
            <MenuRoundedIcon />
          </IconButton>
          <Box sx={{ display: { xs: 'none', sm: 'block' }, flex: 1 }}>
            <Typography variant="body2" color="text.secondary">운영 배포 제어</Typography>
          </Box>
          <Box sx={{ flex: { xs: 1, sm: 0 } }} />
          <Tooltip title="사용자 메뉴">
            <ListItemButton
              aria-label="사용자 메뉴 열기"
              aria-haspopup="menu"
              aria-expanded={Boolean(profileAnchor)}
              onClick={handleProfileMenu}
              sx={{ borderRadius: 2.5, py: 0.65, px: 1, maxWidth: 245 }}
            >
              <Avatar sx={{ width: 38, height: 38, bgcolor: 'primary.dark', fontSize: '0.875rem', fontWeight: 800 }}>{initials}</Avatar>
              <Box sx={{ ml: 1.15, mr: 0.5, minWidth: 0, display: { xs: 'none', sm: 'block' } }}>
                <Typography variant="body2" fontWeight={750} noWrap>{user?.displayName || user?.username}</Typography>
                <Typography variant="caption" color="text.secondary" noWrap>{user?.roles.join(', ') || '사용자'}</Typography>
              </Box>
              <ChevronRightRoundedIcon fontSize="small" sx={{ display: { xs: 'none', sm: 'block' } }} />
            </ListItemButton>
          </Tooltip>
          <Menu
            anchorEl={profileAnchor}
            open={Boolean(profileAnchor)}
            onClose={closeProfile}
            slotProps={{ paper: { sx: { mt: 1, minWidth: 260, maxHeight: 420, overflowY: 'auto' } } }}
          >
            <Box sx={{ px: 2, py: 1.5 }}>
              <Typography fontWeight={750}>{user?.displayName || user?.username}</Typography>
              <Typography variant="body2" color="text.secondary" noWrap>{user?.email || user?.username}</Typography>
            </Box>
            <Divider />
            <MenuItem component={RouterLink} to="/personal/profile" onClick={closeProfile} sx={{ minHeight: 46 }}>
              <ListItemIcon><AccountCircleRoundedIcon fontSize="small" /></ListItemIcon>
              내 프로필
            </MenuItem>
            {hasPermission('keys.manage') && (
              <MenuItem component={RouterLink} to="/personal/api-keys" onClick={closeProfile} sx={{ minHeight: 46 }}>
                <ListItemIcon><KeyRoundedIcon fontSize="small" /></ListItemIcon>
                API 키 관리
              </MenuItem>
            )}
            {canUseSimple && canUseFull && (
              <>
                <Divider />
                <MenuItem
                  onClick={() => {
                    closeProfile();
                    void setMode(mode === 'simple' ? 'full' : 'simple');
                    navigate(mode === 'simple' ? '/' : '/simple');
                  }}
                  sx={{ minHeight: 46 }}
                >
                  <ListItemIcon><TuneRoundedIcon fontSize="small" /></ListItemIcon>
                  {mode === 'simple' ? '전체 모드로 전환' : '심플 모드로 전환'}
                </MenuItem>
              </>
            )}
            <Divider />
            <Box sx={{ px: 2, py: 1.25 }}>
              <Typography variant="caption" color="text.secondary">ReleaseDock v{version.version}</Typography>
            </Box>
            <MenuItem onClick={() => void handleLogout()} sx={{ minHeight: 46, color: 'error.light' }}>
              <ListItemIcon><LogoutRoundedIcon fontSize="small" color="error" /></ListItemIcon>
              로그아웃
            </MenuItem>
          </Menu>
        </Toolbar>
      </AppBar>

      <Box component="aside" sx={{ width: { lg: drawerWidth }, flexShrink: { lg: 0 } }}>
        <Drawer
          variant={desktop ? 'permanent' : 'temporary'}
          open={desktop || mobileOpen}
          onClose={() => setMobileOpen(false)}
          ModalProps={{ keepMounted: true }}
          sx={{ '& .MuiDrawer-paper': { width: drawerWidth, borderRightColor: 'divider' } }}
        >
          {drawer}
        </Drawer>
      </Box>

      <Box
        component="main"
        id="main-content"
        tabIndex={-1}
        sx={{ flex: 1, minWidth: 0, pt: '72px', minHeight: '100vh' }}
      >
        <Box sx={{ width: '100%', maxWidth: 1500, mx: 'auto', px: { xs: 2, sm: 3, xl: 4.5 }, py: { xs: 3, md: 4 } }}>
          <Outlet />
        </Box>
      </Box>
    </Box>
  );
}
