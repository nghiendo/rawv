const API_BASE = 'http://127.0.0.1:8321';

// DOM Elements
const serverStatus = document.getElementById('server-status');
const statusText = serverStatus.querySelector('.text');
const urlInput = document.getElementById('url');
const concurrencyInput = document.getElementById('concurrency');
const concurrencyVal = document.getElementById('concurrency-val');
const sha256Input = document.getElementById('sha256');
const downloadBtn = document.getElementById('download-btn');
const jobsList = document.getElementById('jobs-list');
const advancedToggle = document.getElementById('advanced-toggle');
const advancedContent = document.querySelector('.collapse-content');
const interceptToggle = document.getElementById('intercept-toggle');

let isConnected = false;
let updateInterval = null;

// Initialize
document.addEventListener('DOMContentLoaded', () => {
  checkServerStatus();
  setInterval(checkServerStatus, 3000);

  // Read toggle setting
  if (typeof chrome !== 'undefined' && chrome.storage && chrome.storage.local) {
    chrome.storage.local.get({ autoIntercept: true }, (result) => {
      interceptToggle.checked = result.autoIntercept;
    });
  }

  // Handle toggle change
  interceptToggle.addEventListener('change', (e) => {
    if (typeof chrome !== 'undefined' && chrome.storage && chrome.storage.local) {
      chrome.storage.local.set({ autoIntercept: e.target.checked });
    }
  });

  // Sync range input val
  concurrencyInput.addEventListener('input', (e) => {
    concurrencyVal.textContent = e.target.value;
  });

  // Toggle Advanced Settings
  advancedToggle.addEventListener('click', () => {
    advancedToggle.classList.toggle('open');
    advancedContent.classList.toggle('open');
  });

  // Trigger Download
  downloadBtn.addEventListener('click', startDownload);

  // Poll active jobs
  updateInterval = setInterval(fetchJobs, 1000);
});

// Check local Go Server health status
async function checkServerStatus() {
  try {
    const res = await fetch(`${API_BASE}/api/status`);
    if (res.ok) {
      if (!isConnected) {
        isConnected = true;
        serverStatus.className = 'status-badge connected';
        statusText.textContent = 'Server Connected';
        downloadBtn.disabled = false;
      }
    } else {
      throw new Error();
    }
  } catch (err) {
    isConnected = false;
    serverStatus.className = 'status-badge disconnected';
    statusText.textContent = 'Server Offline';
    downloadBtn.disabled = true;
  }
}

// Start a new download job
async function startDownload() {
  const url = urlInput.value.trim();
  const out = ""; // Auto-default to filename in current directory/downloads
  const concurrency = parseInt(concurrencyInput.value);
  const sha256 = sha256Input.value.trim();

  if (!url) {
    alert('Please enter a valid URL.');
    return;
  }

  try {
    const res = await fetch(`${API_BASE}/api/download`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({ url, out, concurrency, sha256 })
    });

    if (res.ok) {
      urlInput.value = '';
      sha256Input.value = '';
      fetchJobs(); // Update list immediately
    } else {
      const errMsg = await res.text();
      alert(`Failed to start download: ${errMsg}`);
    }
  } catch (err) {
    alert('Error connecting to Go server.');
  }
}

// Fetch all jobs and render them
async function fetchJobs() {
  if (!isConnected) return;

  try {
    const res = await fetch(`${API_BASE}/api/jobs`);
    if (!res.ok) return;

    const jobs = await res.json();
    renderJobs(jobs);
  } catch (err) {
    console.error('Error fetching jobs:', err);
  }
}

// Cancel an ongoing job
async function cancelJob(id) {
  try {
    const res = await fetch(`${API_BASE}/api/jobs/cancel?id=${id}`, {
      method: 'POST'
    });
    if (res.ok) {
      fetchJobs();
    }
  } catch (err) {
    console.error('Error cancelling job:', err);
  }
}

// Render jobs list in UI
function renderJobs(jobs) {
  if (jobs.length === 0) {
    jobsList.innerHTML = `
      <div class="empty-state">
        <p>No active downloads</p>
      </div>
    `;
    return;
  }

  // Sort jobs by newest first (using jobID which is time-based base36 string)
  jobs.sort((a, b) => b.id.localeCompare(a.id));

  jobsList.innerHTML = jobs.map(job => {
    const pct = job.size > 0 ? ((job.downloaded / job.size) * 100).toFixed(1) : '0.0';
    const isRunning = job.status === 'downloading';
    
    // Format values
    const downloadedStr = formatBytes(job.downloaded);
    const totalStr = job.size > 0 ? formatBytes(job.size) : 'Unknown';
    const speedStr = isRunning ? `${formatBytes(job.speed)}/s` : '';
    
    // Status color class
    let statusClass = `status-${job.status}`;

    // Infer display name
    const displayName = job.out_path ? getFilename(job.out_path) : getFilename(job.url);

    return `
      <div class="job-card">
        <div class="job-info">
          <div class="job-title" title="${job.url}">${displayName}</div>
          ${isRunning ? `<button class="job-cancel-btn" data-id="${job.id}">&#x2715;</button>` : ''}
        </div>
        
        <div class="progress-container">
          <div class="progress-bar" style="width: ${pct}%"></div>
        </div>

        <div class="job-stats">
          <div>
            <span class="job-status-text ${statusClass}">${job.status}</span>
            <span style="margin-left: 6px;">${downloadedStr} / ${totalStr} (${pct}%)</span>
          </div>
          <div class="job-speed">${speedStr}</div>
        </div>
        ${job.error ? `<div class="job-stats" style="color: var(--accent-rose); margin-top: 4px; font-size: 9px; max-width: 100%; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;">${job.error}</div>` : ''}
      </div>
    `;
  }).join('');

  // Add click listeners to cancel buttons
  jobsList.querySelectorAll('.job-cancel-btn').forEach(btn => {
    btn.addEventListener('click', () => {
      const id = btn.getAttribute('data-id');
      cancelJob(id);
    });
  });
}

// Utility to format bytes to human readable format
function formatBytes(bytes) {
  if (bytes === 0) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
}

// Utility to extract filename from path or URL
function getFilename(str) {
  try {
    const url = new URL(str);
    const pathname = url.pathname;
    const parts = pathname.split('/');
    const last = parts[parts.length - 1];
    return last || 'downloaded_file';
  } catch (e) {
    const parts = str.split(/[\\/]/);
    return parts[parts.length - 1] || 'downloaded_file';
  }
}
