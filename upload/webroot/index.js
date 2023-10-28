let sort = ['name', 'asc'];

function getCurrentPath() {
    let currentPath = location.hash;
    if (currentPath.startsWith('#')) {
        currentPath = currentPath.substring(1);
    }
    if (currentPath && !currentPath.endsWith('/')) {
        currentPath += '/';
    }
    return currentPath;
}

async function alertError(result) {
    alert('Error: ' + (await result.json()).message);
}

function setSort(col) {
    if (sort[0] === col) {
        sort[1] = sort[1] === 'asc' ? 'desc' : 'asc';
    } else {
        sort = [col, 'asc'];
    }
    load();
}

function formatSort(col) {
    if (sort[0] === col) {
        return sort[1] === 'asc' ? '&#128316;' : '&#128317;';
    }
    return '';
}

async function load() {
    let uplink = '';
    let currentPath = getCurrentPath();
    if (currentPath) {
        const idx = currentPath.lastIndexOf('/', currentPath.length - 2);
        let up = '';
        if (idx > 0) {
            up = currentPath.substring(0, idx);
        }
        uplink = `<div style='text-align: center;'><a href='#${up}'>
            <img src='images/up.png' class='icon' style='margin-bottom: 20px' />
        </a><div>`;
    }
    document.getElementById('path').innerText = decodeURIComponent(currentPath);
    const result = await fetch('files?dir=' + encodeURIComponent(currentPath));
    if (result.status != 200) {
        alertError(result);
        return;
    }
    fetch('stat')
        .then((r) => r.json())
        .then((j) => {
            document.getElementById('stat').innerHTML = `Free space: ${j.free}`;
        });
    const j = await result.json();
    const files = document.getElementById('files');
    files.innerHTML = uplink;
    if (!j.length) {
        files.innerHTML += '<div>Empty</div>';
        return;
    }
    const fileList = document.createElement('table');
    fileList.setAttribute('cellpadding', 5);
    fileList.innerHTML = `<thead><tr><th onclick="setSort('name')" class="clickable">Filename ${formatSort(
        'name'
    )}</th><th onclick="setSort('timestamp')" class="clickable">Upload date ${formatSort(
        'timestamp'
    )}</th></tr></thead>`;
    files.append(fileList);
    const fileListBody = document.createElement('tbody');
    fileList.append(fileListBody);
    j.sort((a, b) => {
        if (a.type !== b.type) {
            return a.type < b.type ? -1 : 1;
        }
        if (a[sort[0]] === b[sort[0]]) {
            return 0;
        }
        let aval = a[sort[0]];
        let bval = b[sort[0]];
        if (typeof aval === 'string') {
            aval = aval.toLowerCase();
            bval = bval.toLowerCase();
        }
        let cmp = aval > bval;
        if (sort[1] === 'desc') {
            cmp = !cmp;
        }
        return cmp ? 1 : -1;
    });
    for (const file of j) {
        const row = document.createElement('tr');
        fileListBody.append(row);
        if (file.type === 'dir') {
            row.innerHTML = `<td colspan="2">
            <a href='#${currentPath + file.name}'>
                <img src='images/folder.png' class='icon' />
                ${file.name}
            </a>
        </td>`;
        } else {
            row.innerHTML += `<td valign="middle">
            <span class="filename"><img src="images/file.png" class="icon" /> ${
                file.name
            }</span></td>
            <td>${new Date(file.timestamp).toLocaleString()}</td>`;
        }
    }
}

async function createDir() {
    const dirName = prompt('Enter new dir name');
    if (!dirName) {
        return;
    }
    let currentPath = getCurrentPath();
    const data = new FormData();
    data.append('dir', currentPath + dirName);
    data.append('type', 'create_dir');
    const result = await fetch('files', { method: 'POST', body: data });
    if (result.status != 200) {
        alertError(result);
        return;
    }
    load();
}

async function uploadFile() {
    const inp = document.createElement('input');
    inp.type = 'file';
    inp.onchange = async function () {
        const loading = document.createElement('div');
        loading.setAttribute('class', 'loading');
        loading.innerHTML =
            '<div class="lds-ring"><div></div><div></div><div></div><div></div></div><div class="animated-progress progress-blue"><span id="progress" /></div><button class="button-3 button-abort" id="abort-btn">Abort</button>';
        document.body.appendChild(loading);
        const progress = document.getElementById('progress');
        const abortBtn = document.getElementById('abort-btn');
        try {
            let currentPath = getCurrentPath();
            const data = new FormData();
            data.append('dir', currentPath);
            data.append('file', inp.files[0]);
            data.append('type', 'upload_file');
            const req = new XMLHttpRequest();
            req.upload.addEventListener('progress', function (ev) {
                if (ev.total > 0) {
                    const perc =
                        ((ev.loaded * 100) / ev.total).toFixed(2) + '%';
                    progress.style.setProperty('width', perc);
                    progress.innerText = perc;
                }
            });
            req.addEventListener('readystatechange', (ev) => {
                if (req.readyState === XMLHttpRequest.DONE) {
                    document.body.removeChild(loading);
                    load();
                    if (req.status !== 200 && req.status !== 0) {
                        alert('Error: ' + JSON.parse(req.response).message);
                    }
                }
            });
            abortBtn.onclick = () => {
                if (window.confirm('Abort upload?')) {
                    req.abort();
                }
            };
            const error = () => {
                alert('Error during file upload');
            };
            req.addEventListener('error', error);
            req.open('POST', 'files');
            req.send(data);
        } finally {
        }
    };
    inp.click();
}

async function downloadFile() {
    const input = document.getElementById('civiturl');
    const url = input.value;
    if (!window.confirm(`Load a LoRA remotely from ${url}?`)) {
        return;
    }
    const params = new FormData();
    params.append('url', url);
    params.append('dir', getCurrentPath());
    fetch('download', { method: 'POST', body: params });
    input.value = '';
    toast('LoRA queued, please wait!', 'success');
}

function toast(text, type) {
    Toastify({
        text,
        duration: 3000,
        close: true,
        gravity: 'bottom',
        position: 'center',
        stopOnFocus: true,
        style: {
            background:
                type === 'error'
                    ? 'linear-gradient(to right, #972c4b, #a42e4f)'
                    : 'linear-gradient(to right, #2c974b, #2ea44f)',
        },
    }).showToast();
}

function startWS() {
    const host = location.protocol.replace('http', 'ws') + '//' + location.host;
    const apiUrl = host + '/q/ws';
    const ws = new WebSocket(apiUrl);
    init(ws);
}

function init(ws) {
    const cont = document.getElementById('dlcontainer');
    const progress = document.getElementById('dlprogress');
    const fn = document.getElementById('dlfilename');
    ws.onopen = () => {
        console.log('Connected!');
    };
    ws.onclose = (e) => {
        setTimeout(() => {
            init(new WebSocket(ws.url));
        }, 1000);
    };
    ws.onmessage = (ev) => {
        if (!ev) {
            return;
        }
        const packet = JSON.parse(ev.data);
        const type = packet.type;
        const data = packet.data;
        switch (type) {
            case 'download':
                if (data.filename) {
                    cont.style.removeProperty('display');
                    fn.innerText = data.filename;
                } else {
                    cont.style.setProperty('display', 'none');
                    load();
                    break;
                }
                const perc =
                    ((data.completed_bytes * 100) / data.total_bytes).toFixed(
                        2
                    ) + '%';
                progress.style.setProperty('width', perc);
                progress.innerText = perc;
                break;
            case 'message':
                toast(data.message, data.type);
        }
    };
}

window.onhashchange = load;
