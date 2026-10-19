'use strict';

const fs = require('node:fs');
const path = require('node:path');

// The desktop shell currently uses only Electron and Node built-ins. No runtime
// node_modules need copying or rebuilding. Return true if that ever changes so
// electron-builder resumes its standard dependency handling automatically.
module.exports = async function beforeBuild({ appDir }) {
  const metadata = JSON.parse(fs.readFileSync(path.join(appDir, 'package.json'), 'utf8'));
  return Object.keys({ ...metadata.dependencies, ...metadata.optionalDependencies }).length > 0;
};
