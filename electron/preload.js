// 预加载脚本：把「选择文件夹」这一件小事暴露给页面，其余能力一概不给。
//
// contextIsolation + sandbox 都开着，页面拿不到 Node；它只能调用这里显式暴露的方法。
// 只暴露一个返回字符串的选目录动作，不暴露任意文件读写或执行命令的能力。
const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('desktop', {
  // 返回选中的绝对路径；用户取消时返回 null。
  pickDirectory: () => ipcRenderer.invoke('pick-directory'),
  getPublicAccessState: () => ipcRenderer.invoke('public-access:get-state'),
  startPublicAccess: () => ipcRenderer.invoke('public-access:start'),
  stopPublicAccess: () => ipcRenderer.invoke('public-access:stop'),
  copyPublicAccessUrl: () => ipcRenderer.invoke('public-access:copy'),
  onPublicAccessState: (callback) => {
    const handler = (_event, state) => callback(state);
    ipcRenderer.on('public-access:state', handler);
    return () => ipcRenderer.removeListener('public-access:state', handler);
  },
  isDesktop: true,
});
