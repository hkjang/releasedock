import { render, screen } from '@testing-library/react';
import { ThemeProvider } from '@mui/material';
import { StatusChip } from './StatusChip';
import { theme } from '../theme';

describe('StatusChip', () => {
  it('renders a visible Korean release status label', () => {
    render(<ThemeProvider theme={theme}><StatusChip status="PENDING_REVIEW" /></ThemeProvider>);
    expect(screen.getByText('검토 대기')).toBeVisible();
  });
});
