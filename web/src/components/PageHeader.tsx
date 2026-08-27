import { Box, Breadcrumbs, Link, Stack, Typography } from '@mui/material';
import { Link as RouterLink } from 'react-router-dom';
import type { ReactNode } from 'react';

export interface Crumb {
  label: string;
  to?: string;
}

export function PageHeader({
  title,
  description,
  action,
  crumbs,
}: {
  title: string;
  description?: string;
  action?: ReactNode;
  crumbs?: Crumb[];
}) {
  return (
    <Box component="header" sx={{ mb: 3.5 }}>
      {crumbs && (
        <Breadcrumbs aria-label="페이지 경로" sx={{ mb: 1 }}>
          {crumbs.map((crumb) =>
            crumb.to ? (
              <Link key={crumb.label} component={RouterLink} to={crumb.to} underline="hover" color="text.secondary">
                {crumb.label}
              </Link>
            ) : (
              <Typography key={crumb.label} color="text.secondary" variant="body2">
                {crumb.label}
              </Typography>
            ),
          )}
        </Breadcrumbs>
      )}
      <Stack direction={{ xs: 'column', sm: 'row' }} alignItems={{ xs: 'stretch', sm: 'flex-start' }} gap={2}>
        <Box sx={{ flex: 1 }}>
          <Typography component="h1" variant="h1">
            {title}
          </Typography>
          {description && (
            <Typography color="text.secondary" sx={{ mt: 0.75, maxWidth: 820 }}>
              {description}
            </Typography>
          )}
        </Box>
        {action}
      </Stack>
    </Box>
  );
}
