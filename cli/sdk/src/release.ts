// The release a process booted from, for app_started: the first non-empty of these, so a
// project whose package.json version never moves (Vercel, Render) still names its deploys.
const RELEASE_VARS = [
  'UPCONTROL_RELEASE',
  'VERCEL_GIT_COMMIT_SHA',
  'RENDER_GIT_COMMIT',
  'RAILWAY_GIT_COMMIT_SHA',
  'HEROKU_SLUG_COMMIT',
  'FLY_IMAGE_REF',
  'K_REVISION',
];

/** releaseFrom reads the release off the environment: the first variable above holding more
 *  than whitespace, trimmed and cut to its first 64 characters (code points, so never half of
 *  a surrogate pair). Undefined when none is set, so the caller omits the field. */
export function releaseFrom(env: NodeJS.ProcessEnv): string | undefined {
  for (const name of RELEASE_VARS) {
    const v = env[name]?.trim();
    if (v) return Array.from(v).slice(0, 64).join('');
  }
  return undefined;
}
