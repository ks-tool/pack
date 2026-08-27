package io.horchestra.pack.sdk;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

import java.util.List;
import java.util.Map;

/** x-pack on the root, with the JSON names of packschema. Nulls, false and empty are omitted. */
@JsonInclude(JsonInclude.Include.NON_EMPTY)
public final class Root {
    public Readiness readiness;
    public Map<String, Peer> peers;
    public Map<String, String> requires;
    public Upgrade upgrade;
    public List<Hook> hooks;
    public List<String> command;
    @JsonProperty("restartPolicy") public String restartPolicy;
    public List<String> savepoints;
    public Rollout rollout;

    public Root readiness(Readiness r) { this.readiness = r; return this; }
    public Root peers(Map<String, Peer> p) { this.peers = p; return this; }
    public Root requires(Map<String, String> r) { this.requires = r; return this; }
    public Root upgrade(String from) { this.upgrade = new Upgrade(from); return this; }
    public Root hooks(List<Hook> h) { this.hooks = h; return this; }
    public Root command(List<String> c) { this.command = c; return this; }
    public Root restartPolicy(String p) { this.restartPolicy = p; return this; }
    public Root savepoints(List<String> s) { this.savepoints = s; return this; }
    public Root rollout(int maxUnavailable) { this.rollout = new Rollout(maxUnavailable); return this; }

    @JsonInclude(JsonInclude.Include.NON_EMPTY)
    public static final class Readiness {
        public String tcp;
        public String http;
        public List<String> exec;
        public static Readiness tcp(String port) { Readiness r = new Readiness(); r.tcp = port; return r; }
        public static Readiness http(String portAndPath) { Readiness r = new Readiness(); r.http = portAndPath; return r; }
    }

    @JsonInclude(JsonInclude.Include.NON_DEFAULT)
    public static final class Peer {
        public String schema;
        public boolean required;
        public Peer(String schema, boolean required) { this.schema = schema; this.required = required; }
    }

    public static final class Upgrade {
        public String from;
        public Upgrade(String from) { this.from = from; }
    }

    @JsonInclude(JsonInclude.Include.NON_EMPTY)
    public static final class When {
        public String from;
        public String to;
        public When(String from, String to) { this.from = from; this.to = to; }
    }

    @JsonInclude(JsonInclude.Include.NON_EMPTY)
    public static final class Step {
        public String image;
        public List<String> command;
        public Step(String image, List<String> command) { this.image = image; this.command = command; }
    }

    @JsonInclude(JsonInclude.Include.NON_DEFAULT)
    public static final class Hook {
        public String name;
        public String phase;
        public When when;
        public String image;
        public List<String> command;
        public boolean exclusive;
        public String timeout;
        public boolean idempotent;
        public Step compensate;
        @JsonProperty("for") public List<String> forKeys;

        public Hook(String name, String phase, List<String> command) { this.name = name; this.phase = phase; this.command = command; }
        public Hook when(String from, String to) { this.when = new When(from, to); return this; }
        public Hook image(String image) { this.image = image; return this; }
        public Hook exclusive() { this.exclusive = true; return this; }
        public Hook timeout(String t) { this.timeout = t; return this; }
        public Hook idempotent() { this.idempotent = true; return this; }
        public Hook compensate(String image, List<String> command) { this.compensate = new Step(image, command); return this; }
        public Hook forKeys(List<String> keys) { this.forKeys = keys; return this; }
    }

    public static final class Rollout {
        @JsonProperty("maxUnavailable") public int maxUnavailable;
        public Rollout(int maxUnavailable) { this.maxUnavailable = maxUnavailable; }
    }
}
