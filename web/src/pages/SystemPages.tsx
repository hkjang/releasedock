import BlockRoundedIcon from '@mui/icons-material/BlockRounded';
import HomeRoundedIcon from '@mui/icons-material/HomeRounded';
import SearchOffRoundedIcon from '@mui/icons-material/SearchOffRounded';
import { Box, Button, Card, CardContent, Typography } from '@mui/material';
import { Link as RouterLink } from 'react-router-dom';

function MessagePage({ forbidden = false }: { forbidden?: boolean }) {
  const Icon = forbidden ? BlockRoundedIcon : SearchOffRoundedIcon;
  return (
    <Box sx={{ minHeight: '60vh', display: 'grid', placeItems: 'center' }}>
      <Card sx={{ width: 'min(100%, 600px)' }}>
        <CardContent sx={{ p: { xs: 3, md: 5 }, textAlign: 'center' }}>
          <Icon color={forbidden ? 'error' : 'primary'} sx={{ fontSize: 54, mb: 2 }} />
          <Typography component="h1" variant="h1">{forbidden ? '접근 권한이 없습니다' : '페이지를 찾을 수 없습니다'}</Typography>
          <Typography color="text.secondary" sx={{ mt: 1.25, mb: 3 }}>
            {forbidden ? '이 메뉴는 서비스 관리자 역할이 필요합니다. 권한이 필요하면 관리자에게 문의하세요.' : '주소가 변경되었거나 삭제된 메뉴입니다.'}
          </Typography>
          <Button component={RouterLink} to="/" variant="contained" startIcon={<HomeRoundedIcon />}>대시보드로 이동</Button>
        </CardContent>
      </Card>
    </Box>
  );
}

export function ForbiddenPage() { return <MessagePage forbidden />; }
export function NotFoundPage() { return <MessagePage />; }
