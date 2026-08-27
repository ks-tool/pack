package io.horchestra.pack.sdk;

import java.util.LinkedHashMap;
import java.util.Map;

/**
 * The platform half of a component's values — replicas, resources,
 * placement — under horchestra's own schema. A field of this type reflects
 * to {@code {"$ref": "horchestra://platform/v1#/$defs/component", "default": …}}
 * with the field's value, pruned of zeros, as the default.
 */
public final class Platform {
    public static final String REF = "horchestra://platform/v1#/$defs/component";

    public int replicas;
    public Resources resources = new Resources();
    public Map<String, Object> placement = new LinkedHashMap<>();

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

    public Platform replicas(int n) {
        this.replicas = n;
        return this;
    }
}
