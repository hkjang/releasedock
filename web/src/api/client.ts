import type {
  ApiKey,
  Application,
  AuditEvent,
  DashboardSummary,
  QuickReleasePreflight,
  DeploymentProfile,
  Environment,
  PageResult,
  Release,
  SettingValue,
  User,
  VersionInfo,
} from '../types/domain';

const API_BASE = '/api/v1';

interface ApiEnvelope<T> {
  data: T;
}

interface ApiErrorEnvelope {
  error: {
    code?: string;
    message: string;
    details?: unknown;
  };
}

export class ApiError extends Error {
  readonly status: number;
  readonly code?: string;
  readonly details?: unknown;

  constructor(message: string, status: number, code?: string, details?: unknown) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

export interface RequestOptions extends Omit<RequestInit, 'body'> {
  body?: BodyInit | object | null;
}

function getCookie(name: string): string | undefined {
  const encodedName = `${encodeURIComponent(name)}=`;
  const item = document.cookie.split(';').map((value) => value.trim()).find((value) => value.startsWith(encodedName));
  return item ? decodeURIComponent(item.slice(encodedName.length)) : undefined;
}

function isApiErrorEnvelope(value: unknown): value is ApiErrorEnvelope {
  return Boolean(
    value &&
      typeof value === 'object' &&
      'error' in value &&
      typeof (value as ApiErrorEnvelope).error?.message === 'string',
  );
}

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const headers = new Headers(options.headers);
  let body = options.body;
  const method = (options.method || 'GET').toUpperCase();

  if (body && !(body instanceof FormData) && !(body instanceof Blob) && typeof body !== 'string') {
    headers.set('Content-Type', 'application/json');
    body = JSON.stringify(body);
  }
  headers.set('Accept', 'application/json');
  if (!['GET', 'HEAD', 'OPTIONS'].includes(method)) {
    const csrfToken = getCookie('releasedock_csrf');
    if (csrfToken) headers.set('X-CSRF-Token', csrfToken);
  }

  const response = await fetch(`${API_BASE}${path}`, {
    ...options,
    headers,
    body: body as BodyInit | null | undefined,
    credentials: 'include',
  });

  const contentType = response.headers.get('content-type') ?? '';
  const payload: unknown = contentType.includes('application/json')
    ? await response.json().catch(() => undefined)
    : await response.text().catch(() => undefined);

  if (!response.ok) {
    if (isApiErrorEnvelope(payload)) {
      throw new ApiError(payload.error.message, response.status, payload.error.code, payload.error.details);
    }
    throw new ApiError(
      typeof payload === 'string' && payload ? payload : `요청을 처리하지 못했습니다. (${response.status})`,
      response.status,
    );
  }

  if (response.status === 204) return undefined as T;
  if (payload && typeof payload === 'object' && 'data' in payload) {
    return (payload as ApiEnvelope<T>).data;
  }
  return payload as T;
}

function toQuery(params: object): string {
  const search = new URLSearchParams();
  Object.entries(params as Record<string, string | number | boolean | undefined>).forEach(([key, value]) => {
    if (value !== undefined && value !== '') search.set(key, String(value));
  });
  const query = search.toString();
  return query ? `?${query}` : '';
}

function toListQuery(params: ListParams = {}): string {
	const pageSize = params.pageSize ?? 50;
	const page = params.page ?? 1;
	return toQuery({
		limit: pageSize,
		offset: Math.max(0, page - 1) * pageSize,
		search: params.search,
		status: params.status,
	});
}

export interface LoginResult {
  user: User;
}

export interface AuthConfig {
  local_enabled: boolean;
  oidc: {
    enabled: boolean;
    issuer?: string;
    /** Try a prompt=none sign-in before showing the login screen. */
    autoLogin?: boolean;
  };
}

export interface ListParams {
  page?: number;
  pageSize?: number;
  search?: string;
  status?: string;
}

export interface NewReleaseInput {
  applicationId: string;
  environmentId: string;
  deploymentProfileId?: string;
  version: string;
  notes?: string;
  artifact: File;
}

export interface QuickReleaseInput {
  artifact: File;
  expectedPresetId: string;
  expectedPresetUpdatedAt: string;
  expectedCurrentVersion: string;
  notes?: string;
}

type WireValue = Record<string, unknown>;

type SettingSection = 'general' | 'oidc' | 'ai' | 'approval' | 'storage' | 'runner' | 'simple' | 'network';

export interface UiModeInfo {
  defaultUiMode: 'simple' | 'full';
  preferredUiMode: '' | 'simple' | 'full';
  effectiveUiMode: 'simple' | 'full';
  canUseSimple: boolean;
  canUseFull: boolean;
  commandMode: 'PER_TARGET' | 'SHARED';
}

export interface SimpleTarget {
  id: string;
  name: string;
  description: string;
  uploadDir: string;
  maxUploadBytes: number;
  ready: boolean;
  notReadyReason: string;
}

export interface SimpleLogLine {
  id: number;
  stream: 'stdout' | 'stderr' | 'system';
  message: string;
  createdAt: string;
}

export interface SimpleRun {
  id: string;
  targetName: string;
  filename: string;
  status: 'PENDING' | 'RUNNING' | 'SUCCESS' | 'FAILED' | 'TIMEOUT';
  exitCode: number | null;
  commandSource: 'PER_TARGET' | 'SHARED';
  commandPath: string;
  commandArgs?: string[];
  sizeBytes: number;
  error?: string;
  storedPath?: string;
  sha256?: string;
  timeoutSeconds?: number;
  actorName?: string;
  createdAt: string;
  startedAt: string | null;
  finishedAt: string | null;
}

function normalizeSettings(section: SettingSection, value: SettingValue): SettingValue {
  if (section === 'general') {
    return {
      ...value,
      serviceName: value.service_name ?? value.serviceName,
      artifactMaxSizeGb: value.artifact_max_bytes !== undefined ? Number(value.artifact_max_bytes) / 1024 ** 3 : value.artifactMaxSizeGb,
    };
  }
  if (section === 'approval') return { ...value, enabled: value.approval_enabled ?? value.enabled };
  if (section === 'storage') {
    return {
      ...value,
      driver: 'local',
      localPath: value.artifact_storage_path ?? value.localPath,
    };
  }
  if (section === 'oidc') {
    const role = value.defaultRole ?? value.defaultRoleId ?? value.default_role_id;
    return {
      ...value,
      enabled: value.enabled,
      issuerUrl: value.issuer ?? value.issuerUrl,
      clientId: value.client_id ?? value.clientId,
      secretConfigured: value.client_secret_configured ?? value.secretConfigured,
      redirectUrl: value.redirect_url ?? value.redirectUrl,
      scopes: Array.isArray(value.scopes) ? value.scopes.join(' ') : value.scopes,
      autoProvision: value.auto_create_user ?? value.autoProvision,
      defaultRole: typeof role === 'string' ? role.replace(/^role-/, '') : role,
    };
  }
  if (section === 'runner' || section === 'simple' || section === 'network') return value;
  return {
    ...value,
    enabled: value.enabled,
    baseUrl: value.endpoint ?? value.baseUrl,
    model: value.model,
    keyConfigured: value.api_key_configured ?? value.keyConfigured,
    maxTokens: value.max_tokens ?? value.maxTokens,
    streamingDefault: true,
  };
}

function serializeSettings(section: SettingSection, value: SettingValue): SettingValue {
	if (section === 'general') {
		return {
			serviceName: value.serviceName,
			artifactMaxSizeGb: Number(value.artifactMaxSizeGb),
			publicUrl: value.publicUrl,
			secureCookies: Boolean(value.secureCookies),
			allowedOrigins: Array.isArray(value.allowedOrigins) ? value.allowedOrigins : [],
		};
	}
	if (section === 'approval') {
		return {
			enabled: Boolean(value.enabled),
			minimumApprovers: 1,
			protectedEnvironments: String(value.protectedEnvironments ?? ''),
			allowSelfApproval: Boolean(value.allowSelfApproval),
			requireRejectComment: value.requireRejectComment === undefined ? true : Boolean(value.requireRejectComment),
		};
	}
	if (section === 'storage') {
		return { driver: 'local', localPath: value.localPath };
  }
  if (section === 'runner') {
    return {
      pollIntervalMs: Number(value.pollIntervalMs),
      lockRetryMs: Number(value.lockRetryMs),
      settingsRefreshMs: Number(value.settingsRefreshMs),
      heartbeatIntervalMs: Number(value.heartbeatIntervalMs),
      staleJobAfterMs: Number(value.staleJobAfterMs),
      logChunkBytes: Number(value.logChunkBytes),
    };
  }
  if (section === 'simple') {
    return {
      defaultUiMode: value.defaultUiMode,
      commandMode: value.commandMode,
      sharedCommandPath: String(value.sharedCommandPath ?? ''),
      sharedCommandArgs: String(value.sharedCommandArgs ?? ''),
      sharedWorkingDir: String(value.sharedWorkingDir ?? ''),
      sharedTimeoutSeconds: Number(value.sharedTimeoutSeconds ?? 600),
      uploadRoot: String(value.uploadRoot ?? ''),
    };
  }
  if (section === 'network') {
    // callerIp is server-derived and must not be echoed back.
    return {
      adminIpAllowlistEnabled: Boolean(value.adminIpAllowlistEnabled),
      adminIpAllowlist: String(value.adminIpAllowlist ?? ''),
      trustedProxyCidrs: String(value.trustedProxyCidrs ?? ''),
    };
  }
  if (section === 'oidc') {
    const result: SettingValue = {
      enabled: Boolean(value.enabled),
      issuerUrl: value.issuerUrl,
      clientId: value.clientId,
      redirectUrl: value.redirectUrl,
      scopes: String(value.scopes ?? ''),
      autoProvision: Boolean(value.autoProvision),
      defaultRole: value.defaultRole || 'viewer',
      allowInsecureEndpoints: Boolean(value.allowInsecureEndpoints),
      autoLogin: Boolean(value.autoLogin),
      verifyTls: true,
    };
    if (value.clientSecret) result.clientSecret = value.clientSecret;
    return result;
  }
  const result: SettingValue = {
    enabled: Boolean(value.enabled),
    baseUrl: value.baseUrl,
    model: value.model,
    maxTokens: Number(value.maxTokens),
    streamingDefault: true,
  };
  if (value.apiKey) result.apiKey = value.apiKey;
  return result;
}

function normalizeApiKey(value: WireValue): ApiKey {
  return {
    id: String(value.id),
    name: String(value.name ?? ''),
    prefix: String(value.prefix ?? ''),
    permissions: (value.permissions ?? value.scopes ?? []) as string[],
    createdAt: String(value.createdAt ?? value.created_at ?? ''),
    expiresAt: value.expiresAt || value.expires_at ? String(value.expiresAt ?? value.expires_at) : undefined,
    lastUsedAt: value.lastUsedAt || value.last_used_at ? String(value.lastUsedAt ?? value.last_used_at) : undefined,
    rotatedAt: value.rotatedAt || value.updated_at ? String(value.rotatedAt ?? value.updated_at) : undefined,
    active: value.active === undefined ? !value.revoked_at : Boolean(value.active),
  };
}

export const api = {
  authConfig: () => request<AuthConfig>('/auth/config'),
  login: (username: string, password: string) =>
    request<LoginResult>('/auth/login', { method: 'POST', body: { username, password } }),
  logout: () => request<void>('/auth/logout', { method: 'POST' }),
  me: () => request<User>('/me'),
  version: () => request<VersionInfo>('/version'),
  dashboard: () => request<DashboardSummary>('/dashboard'),

	releases: (params: ListParams = {}) => request<PageResult<Release>>(`/releases${toListQuery(params)}`),
  release: (id: string) => request<Release>(`/releases/${encodeURIComponent(id)}`),
  updateRelease: (id: string, input: { version: string; notes: string; deploymentProfileId: string }) =>
    request<Release>(`/releases/${encodeURIComponent(id)}`, { method: 'PATCH', body: input }),
  uploadReleaseArtifact: (id: string, artifact: File) => {
    const form = new FormData();
    form.set('artifact', artifact);
    return request(`/releases/${encodeURIComponent(id)}/artifacts/upload`, { method: 'POST', body: form });
  },
  createRelease: (input: NewReleaseInput) => {
    const form = new FormData();
    form.set('applicationId', input.applicationId);
    form.set('environmentId', input.environmentId);
    form.set('version', input.version);
    form.set('artifact', input.artifact);
    if (input.deploymentProfileId) form.set('deploymentProfileId', input.deploymentProfileId);
    if (input.notes) form.set('notes', input.notes);
    return request<Release>('/releases', { method: 'POST', body: form });
  },
  quickReleasePreflight: (filename: string) =>
    request<QuickReleasePreflight>('/releases/preflight', { method: 'POST', body: { filename } }),
  createQuickRelease: (input: QuickReleaseInput) => {
    const form = new FormData();
    form.set('artifact', input.artifact);
    form.set('expectedPresetId', input.expectedPresetId);
    form.set('expectedPresetUpdatedAt', input.expectedPresetUpdatedAt);
    form.set('expectedCurrentVersion', input.expectedCurrentVersion);
    if (input.notes) form.set('notes', input.notes);
    return request<Release>('/releases/quick', { method: 'POST', body: form });
  },
  releaseAction: (id: string, action: 'submit-review' | 'review' | 'approve' | 'reject' | 'deploy' | 'rollback' | 'retry', comment?: string) =>
    request<Release>(`/releases/${encodeURIComponent(id)}/${action}`, {
      method: 'POST',
      body: comment ? { comment } : {},
    }),
  releaseLogStreamUrl: (id: string) => `${API_BASE}/releases/${encodeURIComponent(id)}/logs/stream`,

	applications: () => request<PageResult<Application> | Application[]>('/applications?limit=200&offset=0&activeOnly=true'),
	environments: () => request<PageResult<Environment> | Environment[]>('/environments?limit=200&offset=0&activeOnly=true'),
	profiles: () => request<PageResult<DeploymentProfile> | DeploymentProfile[]>('/deployment-profiles?limit=200&offset=0&activeOnly=true'),

  getResource: async <T>(path: string, params: ListParams = {}) => {
		const response = await request<PageResult<T> | T[]>(`${path}${toListQuery(params)}`);
    if (path !== '/admin/users') return response;
    const normalizeUser = (value: WireValue) => ({
      ...value,
      id: String(value.id),
      displayName: value.displayName ?? value.display_name,
      source: value.source ?? value.auth_source,
      createdAt: value.createdAt ?? value.created_at,
      updatedAt: value.updatedAt ?? value.updated_at,
    }) as T;
    if (Array.isArray(response)) return (response as unknown as WireValue[]).map(normalizeUser);
    return {
      ...response,
      items: (response.items as unknown as WireValue[]).map(normalizeUser),
    } as PageResult<T>;
  },
  createResource: async <T>(path: string, value: Record<string, unknown>) => {
    if (path === '/environments') {
      const applicationId = String(value.applicationId ?? '');
      const { applicationId: _applicationId, ...payload } = value;
      void _applicationId;
      return request<T>(`/applications/${encodeURIComponent(applicationId)}/environments`, { method: 'POST', body: payload });
    }
    if (path === '/admin/roles') {
      const created = await request<WireValue>(path, { method: 'POST', body: { name: value.name, description: value.description } });
      if (Array.isArray(value.permissions) && value.permissions.length) {
        await request(`/admin/roles/${encodeURIComponent(String(created.id))}/permissions`, { method: 'PUT', body: { permissions: value.permissions } });
      }
      return { ...created, permissions: value.permissions ?? [] } as T;
    }
    if (path === '/deployment-profiles' && typeof value.healthChecks === 'string') {
      value = { ...value, healthChecks: JSON.parse(value.healthChecks || '[]') as unknown };
    }
    return request<T>(path, { method: 'POST', body: value });
  },
  updateResource: async <T>(path: string, id: string, value: Record<string, unknown>) => {
    if (path === '/admin/roles') {
      const updated = await request<WireValue>(`${path}/${encodeURIComponent(id)}`, { method: 'PATCH', body: { name: value.name, description: value.description } });
      await request(`${path}/${encodeURIComponent(id)}/permissions`, { method: 'PUT', body: { permissions: value.permissions ?? [] } });
      return { ...updated, permissions: value.permissions ?? [] } as T;
    }
    if (path === '/admin/users') {
      await request(`${path}/${encodeURIComponent(id)}`, { method: 'PATCH', body: { display_name: value.displayName, email: value.email, active: value.active } });
      await request(`${path}/${encodeURIComponent(id)}/roles`, { method: 'PUT', body: { roles: value.roles ?? [] } });
      return { id, ...value } as T;
    }
    if (path === '/environments') {
      const { applicationId: _applicationId, ...payload } = value;
      void _applicationId;
      return request<T>(`${path}/${encodeURIComponent(id)}`, { method: 'PUT', body: payload });
    }
    if (path === '/deployment-profiles' && typeof value.healthChecks === 'string') {
      value = { ...value, healthChecks: JSON.parse(value.healthChecks || '[]') as unknown };
    }
    return request<T>(`${path}/${encodeURIComponent(id)}`, { method: 'PUT', body: value });
  },
  deleteResource: (path: string, id: string) =>
    request<void>(`${path}/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  rotateResourceSecret: (path: string, id: string, secret: string) =>
    request(`${path}/${encodeURIComponent(id)}/rotate`, { method: 'POST', body: { secret } }),

  getSettings: async (section: SettingSection) =>
    normalizeSettings(section, await request<SettingValue>(`/admin/settings/${section}`)),
  saveSettings: async (section: SettingSection, values: SettingValue) =>
    normalizeSettings(section, await request<SettingValue>(`/admin/settings/${section}`, { method: 'PUT', body: serializeSettings(section, values) })),
	users: (params: ListParams = {}) => request<PageResult<User>>(`/admin/users${toListQuery(params)}`),
  audits: async (params: ListParams = {}) => {
		const response = await request<{ items: WireValue[]; total?: number; limit?: number; offset?: number; page?: number; pageSize?: number }>(`/admin/audit${toListQuery(params)}`);
    const pageSize = response.limit ?? params.pageSize ?? 50;
    const page = response.page ?? Math.floor((response.offset ?? ((params.page ?? 1) - 1) * pageSize) / pageSize) + 1;
    const items: AuditEvent[] = response.items.map((value) => ({
      id: String(value.id),
      actor: String(value.actor ?? value.actor_id ?? 'system'),
      action: String(value.action ?? ''),
      resource: [value.resource_type, value.resource_id].filter(Boolean).join(':') || String(value.resource ?? ''),
      result: String(value.result ?? value.outcome ?? ''),
      ipAddress: value.ipAddress || value.ip ? String(value.ipAddress ?? value.ip) : undefined,
      createdAt: String(value.createdAt ?? value.created_at ?? ''),
      detail: value.detail || value.details ? JSON.stringify(value.detail ?? value.details) : undefined,
    }));
    return { items, page, pageSize: response.pageSize ?? pageSize, total: response.total ?? ((page - 1) * pageSize + items.length) };
  },

  permissions: async () => {
    const response = await request<{ items: Array<{ code: string; description: string }> }>('/admin/permissions');
    return response.items ?? [];
  },
  uiMode: () => request<UiModeInfo>('/ui-mode'),
  setPreferredUiMode: (mode: 'simple' | 'full') =>
    request<void>('/me/preferences', { method: 'PATCH', body: { uiMode: mode } }),
  simpleTargets: () => request<{ items: SimpleTarget[]; commandMode: string }>('/simple/targets'),
  simpleRuns: (params: ListParams = {}, mine = false, actor = '') =>
    request<PageResult<SimpleRun>>(
      `/simple/runs${toListQuery(params)}${toListQuery(params) ? '&' : '?'}mine=${mine}${actor ? `&actor=${encodeURIComponent(actor)}` : ''}`,
    ),
  simpleRun: (id: string) => request<SimpleRun>(`/simple/runs/${encodeURIComponent(id)}`),
  simpleRunLogs: (id: string, after = 0) =>
    request<{ items: SimpleLogLine[]; lastId: number; hasMore: boolean }>(
      `/simple/runs/${encodeURIComponent(id)}/logs?after=${after}`,
    ),
  simpleRunLogDownloadUrl: (id: string) => `${API_BASE}/simple/runs/${encodeURIComponent(id)}/logs?format=text`,
  // targetId may be empty: the server uses the only active target, which is
  // what lets the deploy screen accept a bare drag-and-drop.
  startSimpleRun: (targetId: string, artifact: File) => {
    const form = new FormData();
    form.set('artifact', artifact);
    const path = targetId ? `/simple/targets/${encodeURIComponent(targetId)}/runs` : '/simple/runs';
    return request<SimpleRun>(path, { method: 'POST', body: form });
  },
  simpleRunLogStreamUrl: (id: string) => `${API_BASE}/simple/runs/${encodeURIComponent(id)}/logs/stream`,

  updateProfile: (values: Partial<User>) => request<User>('/me/profile', { method: 'PUT', body: values }),
	changePassword: (currentPassword: string, newPassword: string) =>
		request<void>('/me/password', { method: 'PUT', body: { currentPassword, newPassword } }),
  apiKeys: async () => {
    const response = await request<{ items: WireValue[] } | WireValue[]>('/me/api-keys');
    return (Array.isArray(response) ? response : response.items).map(normalizeApiKey);
  },
  createApiKey: (values: { name: string; permissions: string[]; expiresAt?: string }) =>
    request<ApiKey & { secret?: string }>('/me/api-keys', {
      method: 'POST',
      body: { ...values, expiresAt: values.expiresAt ? new Date(`${values.expiresAt}T23:59:59.000Z`).toISOString() : undefined },
    }),
  updateApiKey: (id: string, values: { name?: string; permissions?: string[] }) =>
    request<ApiKey>(`/me/api-keys/${encodeURIComponent(id)}`, { method: 'PATCH', body: values }),
  rotateApiKey: (id: string) =>
    request<ApiKey & { secret: string }>(`/me/api-keys/${encodeURIComponent(id)}/rotate`, { method: 'POST' }),
  revokeApiKey: (id: string) => request<void>(`/me/api-keys/${encodeURIComponent(id)}`, { method: 'DELETE' }),
};

export function unwrapItems<T>(value: PageResult<T> | T[] | undefined): T[] {
  if (!value) return [];
  return Array.isArray(value) ? value : value.items;
}
