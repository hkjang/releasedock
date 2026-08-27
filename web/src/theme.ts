import { alpha, createTheme } from '@mui/material/styles';

export const drawerWidth = 284;

export const theme = createTheme({
  palette: {
    mode: 'dark',
    primary: { main: '#68a9ff', light: '#9bc7ff', dark: '#3479d1' },
    secondary: { main: '#42d6b0' },
    background: { default: '#070b14', paper: '#101827' },
    success: { main: '#4fd1a5' },
    warning: { main: '#f5b942' },
    error: { main: '#ff737a' },
    info: { main: '#67b8ff' },
    divider: alpha('#a9bad3', 0.16),
    text: { primary: '#e8eef8', secondary: '#a9b6c9' },
  },
  typography: {
    fontFamily: 'Inter, Pretendard, "Noto Sans KR", "Apple SD Gothic Neo", "Segoe UI", sans-serif',
    fontSize: 16,
    htmlFontSize: 16,
    h1: { fontSize: '2rem', lineHeight: 1.25, fontWeight: 760, letterSpacing: '-0.025em' },
    h2: { fontSize: '1.5rem', lineHeight: 1.3, fontWeight: 720, letterSpacing: '-0.018em' },
    h3: { fontSize: '1.2rem', lineHeight: 1.4, fontWeight: 700 },
    body1: { fontSize: '1rem', lineHeight: 1.65 },
    body2: { fontSize: '0.9375rem', lineHeight: 1.6 },
    button: { fontSize: '0.9375rem', fontWeight: 700, textTransform: 'none' },
    caption: { fontSize: '0.8125rem', lineHeight: 1.5 },
  },
  shape: { borderRadius: 12 },
  components: {
    MuiCssBaseline: {
      styleOverrides: {
        body: { backgroundImage: 'radial-gradient(circle at 12% -10%, rgba(47, 99, 170, .20), transparent 38%)' },
      },
    },
    MuiButton: {
      defaultProps: { disableElevation: true },
      styleOverrides: { root: { minHeight: 42, borderRadius: 9, paddingInline: 16 } },
    },
    MuiIconButton: {
      styleOverrides: { root: { minWidth: 42, minHeight: 42 } },
    },
    MuiTextField: {
      defaultProps: { size: 'medium' },
    },
    MuiOutlinedInput: {
      styleOverrides: {
        root: { backgroundColor: alpha('#07101f', 0.55) },
        input: { fontSize: '1rem' },
      },
    },
    MuiCard: {
      styleOverrides: {
        root: {
          backgroundImage: 'linear-gradient(145deg, rgba(19, 30, 49, .97), rgba(13, 21, 35, .97))',
          border: `1px solid ${alpha('#b8c9e2', 0.12)}`,
          boxShadow: '0 18px 44px rgba(0, 0, 0, .18)',
        },
      },
    },
    MuiTableCell: {
      styleOverrides: {
        root: { borderBottomColor: alpha('#a9bad3', 0.13), fontSize: '0.9375rem', paddingBlock: 14 },
        head: { color: '#bac7da', fontWeight: 700, backgroundColor: alpha('#07101f', 0.5) },
      },
    },
    MuiChip: {
      styleOverrides: { root: { fontWeight: 700 } },
    },
    MuiTooltip: {
      styleOverrides: { tooltip: { fontSize: '0.8125rem' } },
    },
  },
});
