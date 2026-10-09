package io.horchestra.pack.sdk;

import java.util.LinkedHashMap;
import java.util.Map;

/**
 * The pack's half of a component's platform — resources (cgroups), the same
 * on every target. A field of this type reflects to
 * {@code {"$ref": "urn:pack:platform", "default": …}} with the field's value,
 * pruned of zeros, as the default. Replicas and placement are the target's
 * half: pack schedules nothing.
 */
public final class Platform {
    public static final String REF = "urn:pack:platform";

    public Resources resources = new Resources();

    public static final class Resources {
        public Map<String, String> requests = new LinkedHashMap<>();
        public Map<String, String> limits = new LinkedHashMap<>();
    }

    /** Requests of cpu and memory, a memory limit equal to the request. */
    public static Platform sized(String cpu, String memory) {
        Platform p = new Platform();
        p.resources.requests.put("cpu", cpu);
        p.resources.requests.put("memory", memory);
        p.resources.limits.put("memory", memory);
        return p;
    }

}
