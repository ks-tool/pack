package io.horchestra.pack.sdk;

import java.nio.file.Path;

/** The artifact a component (or a composition) becomes. */
public final class Meta {
    public final String name;
    public final String version;
    /** oci://repo:tag the component runs — no host, the pack's registry; null for a library or a composition. */
    public String image;
    /** oci://repo (no tag, no host) where the artifact is published: what a pack mounts members from. */
    public String artifact;
    /** A directory of template files to ship as templates/. */
    public Path templates;
    public String platform = "linux/amd64";

    public Meta(String name, String version) {
        this.name = name;
        this.version = version;
    }

    public Meta image(String image) { this.image = image; return this; }
    public Meta artifact(String artifact) { this.artifact = artifact; return this; }
    public Meta templates(Path dir) { this.templates = dir; return this; }
    public Meta platform(String platform) { this.platform = platform; return this; }
}
