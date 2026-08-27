import { api } from '../../api/client';
import { buildResourcePayload, loadRelations, resourceConfigs } from './AdminResourcePage';

describe('deployment profile credential permissions', () => {
  it('loads only relation metadata allowed by the caller read permissions', async () => {
    const getResource = vi.spyOn(api, 'getResource').mockResolvedValue({ items: [], total: 0, page: 1, pageSize: 200 });

    await loadRelations(resourceConfigs.profiles, {
      applications: true,
      environments: true,
      registries: false,
      scripts: false,
      targetCredentials: false,
    });

    expect(getResource.mock.calls.map(([path]) => path)).toEqual(['/applications', '/environments']);
  });

  it('requests all profile relation metadata when every read permission is present', async () => {
    const getResource = vi.spyOn(api, 'getResource').mockResolvedValue({ items: [], total: 0, page: 1, pageSize: 200 });

    await loadRelations(resourceConfigs.profiles, {
      applications: true,
      environments: true,
      registries: true,
      scripts: true,
      targetCredentials: true,
    });

    expect(getResource.mock.calls.map(([path]) => path)).toEqual([
      '/applications',
      '/environments',
      '/admin/registries',
      '/admin/scripts',
      '/admin/target-credentials',
    ]);
  });

  it('omits targetCredentialId on create and update without credential-write access', () => {
    const values = {
      name: 'Production',
      applicationId: '11111111-1111-4111-8111-111111111111',
      environmentId: '22222222-2222-4222-8222-222222222222',
      targetCredentialId: '33333333-3333-4333-8333-333333333333',
    };

    expect(buildResourcePayload(resourceConfigs.profiles, values, false, false)).not.toHaveProperty('targetCredentialId');
    expect(buildResourcePayload(resourceConfigs.profiles, values, true, false)).not.toHaveProperty('targetCredentialId');
  });

  it('includes an explicit binding change only with credential-write access', () => {
    const credentialId = '33333333-3333-4333-8333-333333333333';
    const payload = buildResourcePayload(resourceConfigs.profiles, { targetCredentialId: credentialId }, true, true);

    expect(payload.targetCredentialId).toBe(credentialId);
  });
});
