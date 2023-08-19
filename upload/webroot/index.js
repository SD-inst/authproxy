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

async function load() {
    let uplink = '';
    let currentPath = getCurrentPath();
    if (currentPath) {
        const idx = currentPath.lastIndexOf('/', currentPath.length - 2);
        let up = '';
        if (idx > 0) {
            up = currentPath.substring(0, idx);
        }
        uplink = `<a href='#${up}'>
            <img src='images/up.png' class='icon' style='margin-bottom: 20px' />
        </a>`;
    }
    document.getElementById('path').innerText = currentPath;
    const result = await fetch('files?dir=' + currentPath);
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
    for (const file of j) {
        if (file.type === 'dir') {
            files.innerHTML += `<a href='#${currentPath + file.name}'>
        <div class='file-line'>
            <img src='images/folder.png' class='icon' />
            ${file.name}
        </div>
        </a>`;
        } else {
            files.innerHTML += `<div class="file-line">
            <img src="images/file.png" class="icon" />
            ${file.name}
        </div>
        </a>`;
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
