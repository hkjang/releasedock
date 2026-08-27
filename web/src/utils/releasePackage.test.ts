import { parseReleasePackageName } from './releasePackage';

describe('release package filename parser', () => {
  it.each([
    ['appname-v0.2.0.tar.gz', 'appname', '0.2.0'],
    ['ai-portal-v2.4.1-rc1.tar.gz', 'ai-portal', '2.4.1-rc1'],
    ['text2sql-worker-v10.0.3-hotfix1.tar.gz', 'text2sql-worker', '10.0.3-hotfix1'],
    ['portal-api-v1.2.3-alpha.2.tar.gz', 'portal-api', '1.2.3-alpha.2'],
  ])('parses %s', (fileName, artifactPrefix, version) => {
    expect(parseReleasePackageName(fileName)).toEqual({ artifactPrefix, version });
  });

  it.each([
    'appname-0.2.0.tar.gz',
    'appname-v0.2.tar.gz',
    'appname-v01.2.3.tar.gz',
    'appname-v1.2.3-01.tar.gz',
    'appname-v1.2.3-.tar.gz',
    'appname-v1.2.3-alpha..2.tar.gz',
    'appname-v1.2.3+build.1.tar.gz',
    'appname-v1.2.3.zip',
    'AppName-v1.2.3.tar.gz',
    'app_name-v1.2.3.tar.gz',
    'app.name-v1.2.3.tar.gz',
    '../appname-v1.2.3.tar.gz',
    'appname–v1.2.3.tar.gz',
  ])('rejects %s', (fileName) => {
    expect(parseReleasePackageName(fileName)).toBeUndefined();
  });
});
