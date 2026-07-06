const API_BASE = 'http://127.0.0.1:8321';

// Initialize Context Menu on install
chrome.runtime.onInstalled.addListener(() => {
  chrome.contextMenus.create({
    id: "download-with-go",
    title: "Download with Go Downloader",
    contexts: ["link"]
  });
});

// Handle Context Menu clicks
chrome.contextMenus.onClicked.addListener((info, tab) => {
  if (info.menuItemId === "download-with-go" && info.linkUrl) {
    triggerGoDownload(info.linkUrl);
  }
});

// Handle browser download interception
chrome.downloads.onCreated.addListener(async (downloadItem) => {
  // Check if auto-intercept is enabled
  const settings = await chrome.storage.local.get({ autoIntercept: true });
  if (!settings.autoIntercept) return;

  // Skip blobs, data URIs, or downloads already managed/started by localhost
  if (downloadItem.url.startsWith('blob:') || 
      downloadItem.url.startsWith('data:') ||
      downloadItem.url.startsWith('http://127.0.0.1') ||
      downloadItem.url.startsWith('http://localhost')) {
    return;
  }

  // Cancel Chrome's default download
  chrome.downloads.cancel(downloadItem.id);
  // Erase from Chrome's download history to prevent empty entries
  chrome.downloads.erase({ id: downloadItem.id });

  // Trigger Go downloader
  triggerGoDownload(downloadItem.url);
});

async function triggerGoDownload(url) {
  try {
    await fetch(`${API_BASE}/api/download`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({ url: url, concurrency: 4 })
    });
  } catch (err) {
    console.error('Failed to trigger Go downloader:', err);
  }
}
