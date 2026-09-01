import { Button, Card, CardContent, Chip, Stack, Typography } from '@mui/material';
import { Link as RouterLink, useParams } from 'react-router-dom';
import { api } from '../../api/client';
import { PageError, PageLoading } from '../../components/Feedback';
import { PageHeader } from '../../components/PageHeader';
import { GuideBody } from '../../components/GuideBody';
import { useAsync } from '../../hooks/useAsync';
import { formatDate } from '../../utils/format';

const categoryLabels: Record<string, string> = {
  GUIDE: '가이드',
  NOTICE: '공지',
  FAQ: '자주 묻는 질문',
};

export function GuideDetailPage() {
  const { id = '' } = useParams();
  const state = useAsync(() => api.guide(id), [id]);

  if (state.loading && !state.data) return <PageLoading label="가이드를 불러오는 중입니다" />;
  if (state.error) return <PageError error={state.error} onRetry={() => void state.reload()} />;
  if (!state.data) return null;

  const post = state.data;
  return (
    <>
      <PageHeader
        title={post.title}
        description={post.summary || undefined}
        crumbs={[{ label: '사용자 가이드', to: '/guides' }, { label: post.title }]}
      />
      <Card>
        <CardContent sx={{ p: { xs: 2.5, md: 4 } }}>
          <Stack direction="row" alignItems="center" spacing={1} sx={{ mb: 3, flexWrap: 'wrap', gap: 0.75 }}>
            <Chip size="small" label={categoryLabels[post.category] ?? post.category} />
            <Typography variant="caption" color="text.secondary">
              {formatDate(post.updatedAt)}
              {post.author ? ` · ${post.author}` : ''}
            </Typography>
          </Stack>
          <GuideBody body={post.body} />
        </CardContent>
      </Card>
      <Button component={RouterLink} to="/guides" sx={{ mt: 2.5 }}>
        가이드 목록으로
      </Button>
    </>
  );
}
