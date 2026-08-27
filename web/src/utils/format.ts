export function formatDate(value?: string): string {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat('ko-KR', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).format(date);
}

export function formatBytes(value?: number): string {
  if (value === undefined || value < 0) return '—';
  if (value === 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1);
  return `${(value / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}

export function formatDuration(value?: number): string {
  if (value === undefined) return '—';
  if (value < 1000) return `${value}ms`;
  const seconds = Math.round(value / 1000);
  if (seconds < 60) return `${seconds}초`;
  return `${Math.floor(seconds / 60)}분 ${seconds % 60}초`;
}
