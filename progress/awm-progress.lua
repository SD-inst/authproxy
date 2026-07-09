local awful = require("awful")

local function svc_to_code(s, wait_s)
    if not s or s == "" then return "?" end
    s = s:upper()
    if s == "WAIT" then
        if wait_s and wait_s ~= "NONE" then
            return "W" .. svc_to_code(wait_s)
        end
        return "W"
    end
    return s:sub(1, 1)
end

local function parse_json_response(stdout)
    local prog, tq, sq = "0", "0", "0"

    local p = stdout:match('"progress":([0-9.]+)')
    if p then
        prog = tostring(math.floor(tonumber(p) * 100))
    end

    local tq_match = stdout:match('"task_queue":([0-9]+)')
    if tq_match then tq = tq_match end

    local sq_match = stdout:match('"service_queue":([0-9]+)')
    if sq_match then sq = sq_match end

    local svc_match = stdout:match('"service":"([^"]*)"')
    local wait_svc_match = stdout:match('"wait_service":"([^"]*)"')
    local prev_svc_match = stdout:match('"prev_service":"([^"]*)"')
    local prev_wait_svc_match = stdout:match('"prev_wait_service":"([^"]*)"')

    local current_code = svc_to_code(svc_match, wait_svc_match)
    local previous_code = svc_to_code(prev_svc_match, prev_wait_svc_match)

    local svc_display = current_code .. "/" .. previous_code

    local parts = { svc_display .. ":" .. prog .. "%" }
    if tonumber(tq) > 0 then table.insert(parts, string.format("[q: %s]", tq)) end
    if tonumber(sq) > 0 then table.insert(parts, string.format("[sq: %s]", sq)) end
    return table.concat(parts, " ")
end

local function create_widget(host, token)
    host = host or "localhost:7870"
    token = token or ""

    local url = "http://" .. host .. "/q/status.json"
    if token ~= "" then
        url = url .. "?token=" .. token
    end

    return awful.widget.watch(
        "curl -s '" .. url .. "' 2>/dev/null",
        2,
        function(widget, stdout)
            if stdout and #stdout > 0 then
                widget:set_text(parse_json_response(stdout))
            else
                widget:set_text("---")
            end
        end
    )
end

return {
    create_widget = create_widget,
    parse_json_response = parse_json_response,
}
