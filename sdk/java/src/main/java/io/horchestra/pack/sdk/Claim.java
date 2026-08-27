package io.horchestra.pack.sdk;

/** A persistent volume: the mount path is a fact, size and class settings. */
public final class Claim {
    @Pack(kind = Pack.Kind.CONST, json = "mountPath", required = true, pattern = "^/")
    public String mountPath;
    @Pack(kind = Pack.Kind.DEFAULT)
    public String size;
    @Pack(json = "storageClass")
    public String storageClass;

    public Claim() {}

    public static Claim at(String mountPath, String size) {
        Claim c = new Claim();
        c.mountPath = mountPath;
        c.size = size;
        return c;
    }
}
