import { api, ApiError, request, unwrapItems } from './client';

function jsonResponse(value: unknown, status = 200) {
  return new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } });
}

describe('API client', () => {
  it('unwraps the standard data envelope and sends session credentials', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({ data: { version: '1.2.3' } }));
    await expect(api.version()).resolves.toEqual({ version: '1.2.3' });
    expect(fetchMock).toHaveBeenCalledWith('/api/v1/version', expect.objectContaining({ credentials: 'include' }));
  });

  it('converts the standard error envelope into ApiError', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({ error: { code: 'FORBIDDEN', message: '권한이 없습니다.' } }, 403));
    await expect(request('/restricted')).rejects.toMatchObject({ status: 403, code: 'FORBIDDEN', message: '권한이 없습니다.' } satisfies Partial<ApiError>);
  });

  it('sends the CSRF cookie on unsafe requests', async () => {
    document.cookie = 'releasedock_csrf=test-csrf-token; path=/';
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({ data: null }));
    await request('/test', { method: 'POST', body: {} });
    const [, options] = fetchMock.mock.calls[0];
    expect(new Headers(options?.headers).get('X-CSRF-Token')).toBe('test-csrf-token');
    expect(new Headers(options?.headers).get('Authorization')).toBeNull();
  });

  it('normalizes arrays and paged results', () => {
    expect(unwrapItems([{ id: '1' }])).toEqual([{ id: '1' }]);
    expect(unwrapItems({ items: [{ id: '2' }], total: 1, page: 1, pageSize: 20 })).toEqual([{ id: '2' }]);
  });

	it('accepts legacy snake_case settings and writes the current camelCase contract', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch')
      .mockResolvedValueOnce(jsonResponse({ enabled: true, endpoint: 'http://ai.internal/v1', model: 'local-model', api_key_configured: true, max_tokens: 262144 }))
      .mockResolvedValueOnce(jsonResponse({ enabled: true, endpoint: 'http://ai.internal/v1', model: 'local-model', api_key_configured: true, max_tokens: 262144 }));

    await expect(api.getSettings('ai')).resolves.toMatchObject({ baseUrl: 'http://ai.internal/v1', keyConfigured: true, maxTokens: 262144, streamingDefault: true });
    await api.saveSettings('ai', { enabled: true, baseUrl: 'http://ai.internal/v1', model: 'local-model', apiKey: '', maxTokens: 262144, streamingDefault: true });
    const [, options] = fetchMock.mock.calls[1];
    expect(JSON.parse(String(options?.body))).toEqual({ enabled: true, baseUrl: 'http://ai.internal/v1', model: 'local-model', maxTokens: 262144, streamingDefault: true });
	});

	it('writes general and storage settings without legacy fields', async () => {
		const fetchMock = vi.spyOn(globalThis, 'fetch')
			.mockResolvedValueOnce(jsonResponse({ serviceName: 'ReleaseDock', artifactMaxSizeGb: 30 }))
			.mockResolvedValueOnce(jsonResponse({ driver: 'local', localPath: '/srv/releasedock/artifacts' }));
		await api.saveSettings('general', {
			service_name: 'legacy', serviceName: 'ReleaseDock', artifact_max_bytes: 1, artifactMaxSizeGb: 30,
			publicUrl: 'https://releasedock.internal', secureCookies: true, allowedOrigins: ['https://mcp.internal'],
		});
		await api.saveSettings('storage', {
			artifact_storage_path: '/legacy', driver: 'local', localPath: '/srv/releasedock/artifacts',
		});
		expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({ serviceName: 'ReleaseDock', artifactMaxSizeGb: 30, publicUrl: 'https://releasedock.internal', secureCookies: true, allowedOrigins: ['https://mcp.internal'] });
		expect(JSON.parse(String(fetchMock.mock.calls[1][1]?.body))).toEqual({ driver: 'local', localPath: '/srv/releasedock/artifacts' });
	});

	it('preserves the configurable conditional approval policy', async () => {
		const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({ enabled: true }));
		await api.saveSettings('approval', {
			enabled: true,
			protectedEnvironments: 'PRD, DR',
			allowSelfApproval: false,
			requireRejectComment: true,
		});
		expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({
			enabled: true,
			minimumApprovers: 1,
			protectedEnvironments: 'PRD, DR',
			allowSelfApproval: false,
			requireRejectComment: true,
		});
	});

  it('writes only the supported Runner tuning fields', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({ pollIntervalMs: 1500, updatedAt: '2026-01-01T00:00:00Z' }));
    await api.saveSettings('runner', {
      pollIntervalMs: 1500,
      lockRetryMs: 5000,
      settingsRefreshMs: 30000,
      heartbeatIntervalMs: 5000,
      staleJobAfterMs: 60000,
      logChunkBytes: 16384,
      updatedAt: 'must-not-be-written',
    });
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({
      pollIntervalMs: 1500,
      lockRetryMs: 5000,
      settingsRefreshMs: 30000,
      heartbeatIntervalMs: 5000,
      staleJobAfterMs: 60000,
      logChunkBytes: 16384,
    });
  });

  it('maps UI pagination to the backend limit and offset contract', async () => {
		const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({ items: [], total: 0, page: 2, pageSize: 25 }));
		await api.releases({ page: 2, pageSize: 25, status: 'SUCCESS' });
		expect(String(fetchMock.mock.calls[0][0])).toBe('/api/v1/releases?limit=25&offset=25&status=SUCCESS');
	});

  it('uses the filename-only preflight contract', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({ data: { filename: 'ai-portal-v2.4.1.tar.gz' } }));
    await api.quickReleasePreflight('ai-portal-v2.4.1.tar.gz');
    const [url, options] = fetchMock.mock.calls[0];
    expect(String(url)).toBe('/api/v1/releases/preflight');
    expect(options?.method).toBe('POST');
    expect(JSON.parse(String(options?.body))).toEqual({ filename: 'ai-portal-v2.4.1.tar.gz' });
  });

  it('uploads only the artifact and optional notes for a quick release', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({ data: { id: 'release-1' } }, 201));
    const artifact = new File(['package'], 'ai-portal-v2.4.1.tar.gz', { type: 'application/gzip' });
    await api.createQuickRelease({ artifact, expectedPresetId: 'preset-1', expectedPresetUpdatedAt: '2026-08-27T00:00:00Z', expectedCurrentVersion: '2.4.0', notes: 'security fixes' });
    const [url, options] = fetchMock.mock.calls[0];
    expect(String(url)).toBe('/api/v1/releases/quick');
    expect(options?.method).toBe('POST');
    const body = options?.body as FormData;
    expect(body.get('artifact')).toBe(artifact);
    expect(body.get('expectedPresetId')).toBe('preset-1');
    expect(body.get('expectedPresetUpdatedAt')).toBe('2026-08-27T00:00:00Z');
    expect(body.get('expectedCurrentVersion')).toBe('2.4.0');
    expect(body.get('notes')).toBe('security fixes');
    expect([...body.keys()]).toEqual(['artifact', 'expectedPresetId', 'expectedPresetUpdatedAt', 'expectedCurrentVersion', 'notes']);
  });

  it('preserves server pagination while normalizing admin users', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({
      items: [{ id: 'u1', username: 'operator', displayName: '운영자', source: 'local', createdAt: '2026-01-01T00:00:00Z' }],
      total: 37,
      page: 2,
      pageSize: 25,
    }));
    await expect(api.getResource('/admin/users', { page: 2, pageSize: 25, search: '운영' })).resolves.toMatchObject({
      items: [{ id: 'u1', displayName: '운영자', source: 'local', createdAt: '2026-01-01T00:00:00Z' }],
      total: 37,
      page: 2,
      pageSize: 25,
    });
  });

  it('uses the exact audit total returned by the server', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({
      items: [{ id: 1, actor: 'admin', action: 'settings.update', outcome: 'success', created_at: '2026-01-01T00:00:00Z' }],
      total: 501,
      limit: 50,
      offset: 50,
      page: 2,
      pageSize: 50,
    }));
    await expect(api.audits({ page: 2, pageSize: 50 })).resolves.toMatchObject({ total: 501, page: 2, pageSize: 50 });
  });

  it('normalizes API key scope and timestamp fields', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({ items: [{ id: 'k1', name: 'MCP', prefix: 'rdk_1234', scopes: ['mcp.use'], created_at: '2026-01-01T00:00:00Z', revoked_at: null }] }));
    await expect(api.apiKeys()).resolves.toEqual([{ id: 'k1', name: 'MCP', prefix: 'rdk_1234', permissions: ['mcp.use'], createdAt: '2026-01-01T00:00:00Z', active: true }]);
  });
});
