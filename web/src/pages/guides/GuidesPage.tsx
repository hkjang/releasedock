import { Card, CardActionArea, CardContent, Chip, Stack, Typography } from '@mui/material';
import PushPinRoundedIcon from '@mui/icons-material/PushPinRounded';
import { Link as RouterLink } from 'react-router-dom';
import { api } from '../../api/client';
import { EmptyState, PageError, PageLoading } from '../../components/Feedback';
import { PageHeader } from '../../components/PageHeader';
import { useAsync } from '../../hooks/useAsync';
import { formatDate } from '../../utils/format';

const categoryLabels: Record<string, string> = {
  GUIDE: '가이드',
  NOTICE: '공지',
  FAQ: '자주 묻는 질문',
};

export function GuidesPage() {
  const state = useAsync(() => api.guides(), []);

  return (
    <>
      <PageHeader title="사용자 가이드" description="배포 절차와 자주 묻는 질문을 안내합니다." />
      {state.loading && !state.data && <PageLoading label="가이드를 불러오는 중입니다" />}
      {state.error && <PageError error={state.error} onRetry={() => void state.reload()} />}
      {state.data && (
        state.data.items.length ? (
          <Stack spacing={2}>
            {state.data.items.map((post) => (
              <Card key={post.id}>
                <CardActionArea component={RouterLink} to={`/guides/${encodeURIComponent(post.id)}`}>
                  <CardContent>
                    <Stack direction="row" alignItems="center" spacing={1} sx={{ mb: 0.75, flexWrap: 'wrap', gap: 0.75 }}>
                      {post.pinned && <PushPinRoundedIcon sx={{ fontSize: 16, color: 'primary.light' }} />}
                      <Chip size="small" label={categoryLabels[post.category] ?? post.category} />
                      <Typography variant="caption" color="text.secondary">
                        {formatDate(post.updatedAt)}
                        {post.author ? ` · ${post.author}` : ''}
                      </Typography>
                    </Stack>
                    <Typography variant="h3" sx={{ mb: post.summary ? 0.75 : 0 }}>{post.title}</Typography>
                    {Boolean(post.summary) && (
                      <Typography color="text.secondary">{post.summary}</Typography>
                    )}
                  </CardContent>
                </CardActionArea>
              </Card>
            ))}
          </Stack>
        ) : (
          <Card>
            <EmptyState
              title="등록된 가이드가 없습니다"
              description="관리자가 가이드 게시글을 등록하면 여기에 표시됩니다."
            />
          </Card>
        )
      )}
    </>
  );
}
