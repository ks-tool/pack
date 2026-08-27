package io.horchestra.pack.sdk;

/**
 * The type of a credential leaf: {@code format: password} — the operator gives
 * a Secret reference, never a value. Declare {@code Secret password;} and,
 * with {@code @Pack(env = "…")} or under a {@code @File} subtree, where it goes.
 */
public final class Secret {
    private Secret() {}
}
