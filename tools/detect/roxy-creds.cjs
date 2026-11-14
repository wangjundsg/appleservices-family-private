// Roxy/Forge credentials loader. Never hard-code the token in a tracked file:
// it lives in the environment or in .cache/roxy-creds.json (gitignored).
//
//   ROXY_TOKEN=... ROXY_WORKSPACE_ID=... ROXY_PROJECT_ID=... node ...
//   or .cache/roxy-creds.json: {"token":"...","workspaceId":"...","projectId":"..."}
const fs = require('node:fs');
const path = require('node:path');

function load() {
  const fromEnv = {
    token: process.env.ROXY_TOKEN || '',
    workspaceId: process.env.ROXY_WORKSPACE_ID || '',
    projectId: process.env.ROXY_PROJECT_ID || '',
  };
  if (fromEnv.token && fromEnv.workspaceId) return { ...fromEnv, source: 'env' };

  const file = path.resolve(__dirname, '..', '..', '.cache', 'roxy-creds.json');
  if (fs.existsSync(file)) {
    const json = JSON.parse(fs.readFileSync(file, 'utf8'));
    if (json.token && json.workspaceId) return { ...json, source: file };
  }
  throw new Error(
    'ROXY_CREDS_MISSING: export ROXY_TOKEN/ROXY_WORKSPACE_ID/ROXY_PROJECT_ID, '
    + 'or create .cache/roxy-creds.json {"token":"...","workspaceId":"...","projectId":"..."}',
  );
}

module.exports = { load };
