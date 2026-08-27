import { CssBaseline, ThemeProvider } from '@mui/material';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './app/App';
import './styles.css';
import { theme } from './theme';

const root = document.getElementById('root');
if (!root) throw new Error('ReleaseDock root element is missing.');

createRoot(root).render(
  <StrictMode>
    <ThemeProvider theme={theme}>
      <CssBaseline />
      <App />
    </ThemeProvider>
  </StrictMode>,
);
