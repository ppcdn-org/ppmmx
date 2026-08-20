obs = obslua
source_name = "msClock"

function set_time_text()
    local text = os.date("%Y-%m-%d %H:%M:%S.") .. string.format("%03d", math.floor(os.clock() * 1000) % 1000)
    local source = obs.obs_get_source_by_name(source_name)
    if source ~= nil then
        local settings = obs.obs_data_create()
        obs.obs_data_set_string(settings, "text", text)
        obs.obs_source_update(source, settings)
        obs.obs_data_release(settings)
        obs.obs_source_release(source)
    end
end

function timer_callback()
    set_time_text()
end

function script_properties()
    local props = obs.obs_properties_create()
    obs.obs_properties_add_list(props, "source", "Text Source", obs.OBS_COMBO_TYPE_EDITABLE, obs.OBS_COMBO_FORMAT_STRING)
    return props
end

function script_update(settings)
    source_name = obs.obs_data_get_string(settings, "source")
    obs.timer_remove(timer_callback)
    obs.timer_add(timer_callback, 1)  -- 每毫秒更新
end

function script_load(settings)
end