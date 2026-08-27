export type Identifier = string;

export type UserRole = 'admin' | 'operator' | 'approver' | 'developer' | 'viewer' | string;

export interface User {
  id: Identifier;
  username: string;
  displayName: string;
  email?: string;
  roles: UserRole[];
  permissions?: string[];
  source?: 'local' | 'oidc';
  active?: boolean;
  lastLoginAt?: string;
}

export interface VersionInfo {
  version: string;
  commit?: string;
  builtAt?: string;
}

export type ReleaseStatus =
  | 'DRAFT'
  | 'UPLOADED'
  | 'VALIDATING'
  | 'PRE_CHECK'
  | 'READY'
  | 'QUEUED'
  | 'PENDING_REVIEW'
  | 'UNDER_REVIEW'
  | 'APPROVED'
  | 'REJECTED'
  | 'EXTRACTING'
  | 'IMAGE_IMPORT'
  | 'IMAGE_INSPECT'
  | 'IMAGE_LOAD'
  | 'IMAGE_TAG'
  | 'IMAGE_PUSH'
  | 'DEPLOYING'
  | 'VERIFYING'
  | 'SUCCESS'
  | 'FAILED'
  | 'ROLLBACK'
  | 'ROLLED_BACK';

export type StepStatus = 'PENDING' | 'RUNNING' | 'SUCCESS' | 'FAILED' | 'SKIPPED';

export interface ReleaseStep {
  id: Identifier;
  name: string;
  type: string;
  status: StepStatus;
  startedAt?: string;
  finishedAt?: string;
  durationMs?: number;
  exitCode?: number;
  message?: string;
}

export interface ReleaseImage {
  id?: Identifier;
  repository: string;
  tag: string;
  digest?: string;
  size?: number;
}

export interface Release {
  id: Identifier;
  version: string;
  status: ReleaseStatus;
  applicationId: Identifier;
  applicationName?: string;
  environmentId: Identifier;
  environmentName?: string;
  deploymentProfileId?: Identifier;
  deploymentProfileName?: string;
  requestedOperation?: 'DEPLOY' | 'ROLLBACK';
  rollbackSourceReleaseId?: Identifier;
  rollbackSourceVersion?: string;
  rollbackEligible?: boolean;
  retryEligible?: boolean;
  artifactName?: string;
  artifactSize?: number;
  checksum?: string;
  notes?: string;
  createdBy?: User;
  createdAt: string;
  updatedAt?: string;
  startedAt?: string;
  finishedAt?: string;
  steps?: ReleaseStep[];
  images?: ReleaseImage[];
  approval?: {
    required: boolean;
    status?: 'PENDING' | 'APPROVED' | 'REJECTED';
    reviewer?: User;
    comment?: string;
    reviewedAt?: string;
  };
}

export interface Application {
  id: Identifier;
  name: string;
  code: string;
  description?: string;
  active?: boolean;
  createdAt?: string;
}

export interface Environment {
  id: Identifier;
  applicationId?: Identifier;
  name: string;
  code: string;
  kind?: 'DEV' | 'STG' | 'PRD' | string;
  description?: string;
  protected?: boolean;
  active?: boolean;
}

export interface DeploymentProfile {
  id: Identifier;
  name: string;
  applicationId?: Identifier;
  environmentId?: Identifier;
  registryId?: Identifier;
  timeoutSeconds?: number;
  approvalRequired?: boolean;
  active?: boolean;
}

export interface DeploymentPreset {
  id: Identifier;
  name: string;
  artifactPrefix: string;
  applicationId: Identifier;
  environmentId: Identifier;
  deploymentProfileId: Identifier;
  autoDeployAfterApproval: boolean;
  active: boolean;
  applicationName?: string;
  environmentName?: string;
  deploymentProfileName?: string;
  createdAt?: string;
  updatedAt?: string;
}

export interface QuickReleasePreflight {
  filename: string;
  artifactPrefix: string;
  version: string;
  currentVersion?: string | null;
  approvalRequired: boolean;
  nextAction: 'APPROVAL' | 'DEPLOY';
  preset: {
    id: Identifier;
    name: string;
    autoDeployAfterApproval: boolean;
    updatedAt: string;
  };
  application: {
    id: Identifier;
    code: string;
    name: string;
  };
  environment: {
    id: Identifier;
    code: string;
    name: string;
    kind?: string;
  };
  deploymentProfile: {
    id: Identifier;
    name: string;
  };
  readiness: {
    profileReady: boolean;
    runnerAvailable: boolean;
  };
}

export interface ApiKey {
  id: Identifier;
  name: string;
  prefix: string;
  permissions: string[];
  createdAt: string;
  expiresAt?: string;
  lastUsedAt?: string;
  rotatedAt?: string;
  active: boolean;
}

export interface AuditEvent {
  id: Identifier;
  actor: string;
  action: string;
  resource: string;
  result: 'SUCCESS' | 'FAILURE' | string;
  ipAddress?: string;
  createdAt: string;
  detail?: string;
}

export interface PageResult<T> {
  items: T[];
  total: number;
  page: number;
  pageSize: number;
}

export interface DashboardSummary {
  totalReleases: number;
  activeDeployments: number;
  pendingApprovals: number;
  successRate: number;
  recentReleases: Release[];
}

export interface SettingValue {
  [key: string]: unknown;
}
