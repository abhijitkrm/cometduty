
async function loadState() {
   const enableLogs = await fetch("logsenabled", {
        method: 'GET', mode: 'cors', cache: 'no-cache',
        credentials: 'same-origin', redirect: 'error', referrerPolicy: 'no-referrer'
    });
    let showLog
    try { showLog = await enableLogs.json() } catch(e) { console.log(e) }
    if (showLog.enabled === false) {
        document.getElementById("logContainer").hidden = true
    }
    const response = await fetch("state", {
        method: 'GET', mode: 'cors', cache: 'no-cache',
        credentials: 'same-origin', redirect: 'error', referrerPolicy: 'no-referrer'
    });
    let initialState
    try { initialState = await response.json() } catch(e) { console.log(e) }
    if (initialState && initialState.Status) {
        updateTable(initialState)
        drawSeries(initialState)
    }
    const logResponse = await fetch("logs", {
        method: 'GET', mode: 'cors', cache: 'no-cache',
        credentials: 'same-origin', redirect: 'error', referrerPolicy: 'no-referrer'
    });
    let initialLogs
    try { initialLogs = await logResponse.json() } catch(e) { console.log(e) }
    if (Array.isArray(initialLogs)) {
        for (let i = initialLogs.length-1; i >= 0; i--) {
            if (initialLogs[i].ts === 0) { addLogMsg(""); continue }
            addLogMsg(`${new Date(initialLogs[i].ts*1000).toLocaleTimeString()} - ${initialLogs[i].msg}`)
        }
    }
}

const blocks = new Map();
function updateTable(status) {
    for (let i = document.getElementById("statusTable").rows.length; i > 0; i--) {
        document.getElementById("statusTable").deleteRow(i-1)
    }
    const fade = `uk-animation-scale-up`
    for (let i = 0; i < status.Status.length; i++) {
        const row = status.Status[i]

        let alerts = "&nbsp;"
        if (row.active_alerts > 0 || row.last_error !== "") {
            if (row.last_error !== "") {
                // use the row index for the modal id — never the chain name,
                // which could contain characters that break the attribute
                alerts = `
            <a href="#modal-center-${i}" uk-toggle><span uk-icon='warning' uk-tooltip="${_.escape(String(row.active_alerts))} active issues" style='color: darkorange'></span></a>
            <div id="modal-center-${i}" class="uk-flex-top" uk-modal>
                <div class="uk-modal-dialog uk-modal-body uk-margin-auto-vertical uk-background-secondary">
                    <button class="uk-modal-close-default" type="button" uk-close></button>
                    <pre class=" uk-background-secondary" style="color: white">${_.escape(row.last_error)}</pre>
                </div>
            </div>
            `
            } else {
                alerts = `<span uk-icon='warning' uk-tooltip="${_.escape(String(row.active_alerts))} active issues" style='color: darkorange'></span>`
            }
        }

        let bonded = ""
        switch (true) {
            case row.tombstoned:
                bonded = "<div class='uk-text-warning'><span uk-icon='ban'></span> <strong>Tombstoned</strong></div>"
                break
            case row.jailed:
                bonded = "<span uk-icon='warning'></span> <strong>Jailed</strong>"
                break
            case row.bonded:
                bonded = "<span uk-icon='check'></span>"
                break
            default:
                bonded = "<span uk-icon='minus-circle'></span> Not active"
        }

        let window = `<div class="uk-width-1-2" style="text-align: end">`
        if (row.missed === 0 && row.window === 0) {
            window += "n/a</div>"
        } else if (row.missed === 0) {
            window += `100%</div>`
        } else {
            window += `${(100 - (row.missed / row.window) * 100).toFixed(2)}%</div>`
        }
        window += `<div class="uk-width-1-2">${_.escape(String(row.missed))} / ${_.escape(String(row.window))}</div>`

        let nodes = `${_.escape(String(row.healthy_nodes))} / ${_.escape(String(row.nodes))}`
        if (row.healthy_nodes < row.nodes) {
            nodes = "<strong><span uk-icon='arrow-down' style='color: darkorange'></span>" + nodes + "</strong>"
        }

        let heightClass = ""
        const heightKey = row.chain_id + "|" + (row.validator || "")
        if (blocks.get(heightKey) !== row.height){
            heightClass = fade
        }
        blocks.set(heightKey, row.height)

        // for chains with multiple validators show a short valcons suffix
        let monikerCell = _.escape(String(row.moniker || "").substring(0,24))
        if (row.validator) {
            monikerCell += ` <span class="uk-text-muted" style="font-size: 0.8em">${_.escape(String(row.validator).substring(0,14))}…</span>`
        }

        let r=document.getElementById('statusTable').insertRow(i)
        r.insertCell(0).innerHTML = `<div>${alerts}</div>`
        r.insertCell(1).innerHTML = `<div>${_.escape(row.name)} (${_.escape(row.chain_id)})</div>`
        r.insertCell(2).innerHTML = `<div class="${heightClass}" style="font-family: monospace; color: #6f6f6f; text-align: start">${_.escape(String(row.height))}</div>`
        if (row.moniker === "not connected") {
            r.insertCell(3).innerHTML = `<div class="uk-text-warning">${_.escape(row.moniker)}</div>`
            bonded = "unknown"
        } else {
            r.insertCell(3).innerHTML = `<div class='uk-text-truncate'>${monikerCell}</div>`
        }
        r.insertCell(4).innerHTML = `<div style="text-align: center">${bonded}</div>`
        r.insertCell(5).innerHTML = `<div uk-grid>${window}</div>`
        r.insertCell(6).innerHTML = `<div class="uk-text-center">${nodes}</div>`
    }
}

let logs = new Array(1);
function addLogMsg(str) {
    if (logs.length >= 256) {
        logs.pop()
    }
    logs.unshift(str)
    if (document.visibilityState !== "hidden") {
        document.getElementById("logs").innerText = logs.join("\n")
    }
}

function connect() {
    let wsProto = "ws://"
    if (location.protocol === "https:") {
        wsProto = "wss://"
    }
    const parse = function (event) {
        const msg = JSON.parse(event.data);
        if (msg.msgType === "log"){
            const e = msg.entry || msg
            addLogMsg(`${new Date(e.ts*1000).toLocaleTimeString()} - ${e.msg}`)
        } else if (msg.msgType === "update" && document.visibilityState !== "hidden"){
            updateTable(msg)
            drawSeries(msg)
        }
        event = null
    }
    const socket = new WebSocket(wsProto + location.host + '/ws');
    socket.addEventListener('message', function (event) {parse(event)});
    socket.onclose = function(e) {
        console.log('Socket is closed, retrying /ws ...', e.reason);
        addLogMsg('Socket is closed, retrying /ws ...' + e.reason)
        setTimeout(function() {
            connect();
        }, 3000);
    };
}
connect()
