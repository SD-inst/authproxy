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
            '<div class="lds-ring"><div></div><div></div><div></div><div></div></div>';
        document.body.appendChild(loading);
        try {
            let currentPath = getCurrentPath();
            const data = new FormData();
            data.append('dir', currentPath);
            data.append('file', inp.files[0]);
            data.append('type', 'upload_file');
            const result = await fetch('files', { method: 'POST', body: data });
            if (result.status != 200) {
                alertError(result);
                return;
            }
        } finally {
            document.body.removeChild(loading);
            load();
        }
    };
    inp.click();
}

window.onhashchange = load;
