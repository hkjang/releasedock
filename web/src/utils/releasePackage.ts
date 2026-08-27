export interface ParsedReleasePackage {
  artifactPrefix: string;
  version: string;
}

// The server remains authoritative. This parser exists to give immediate, offline
// feedback before the package metadata is sent to the preset resolver.
const PACKAGE_NAME = /^([a-z0-9](?:[a-z0-9-]*[a-z0-9])?)-v((?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?)\.tar\.gz$/;

export function parseReleasePackageName(fileName: string): ParsedReleasePackage | undefined {
  if (fileName.length > 256) return undefined;
  const match = PACKAGE_NAME.exec(fileName);
  if (!match) return undefined;
  if (match[1].length > 64 || match[2].length > 128) return undefined;
  const prerelease = match[3];
  if (prerelease?.split('.').some((part) => /^\d+$/.test(part) && part.length > 1 && part.startsWith('0'))) return undefined;
  return { artifactPrefix: match[1], version: match[2] };
}
