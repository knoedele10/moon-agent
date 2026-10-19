const fs = require('fs');
const path = require('path');

// electron-builder's parallel extraResources copy can race while creating
// the nested skill template directories on Windows. Copy this tree in order
// after the rest of the app has been packed.
function copyTree(source, destination) {
  fs.mkdirSync(destination, { recursive: true });
  for (const item of fs.readdirSync(source, { withFileTypes: true })) {
    const from = path.join(source, item.name);
    const to = path.join(destination, item.name);
    if (item.isDirectory()) {
      copyTree(from, to);
    } else if (item.isFile()) {
      fs.copyFileSync(from, to);
    } else {
      throw new Error(`Unsupported skill entry: ${from}`);
    }
  }
}

module.exports = async function afterPack(context) {
  const root = path.resolve(__dirname, '..');
  const source = path.join(root, 'skills');
  const resources = context.electronPlatformName === 'darwin'
    ? path.join(context.appOutDir, `${context.packager.appInfo.productFilename}.app`, 'Contents', 'Resources')
    : path.join(context.appOutDir, 'resources');
  copyTree(source, path.join(resources, 'skills'));
};
