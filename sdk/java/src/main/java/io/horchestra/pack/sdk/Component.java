package io.horchestra.pack.sdk;

import java.util.Map;

/**
 * An author's component: a class whose public fields, with their values, are
 * the defaults document. Only {@link #meta()} is required; {@link #root()}
 * and {@link #compute()} when the component has something to say.
 */
public interface Component {
    Meta meta();

    /** x-pack on the root: readiness, peers, requires, upgrade, hooks, command, restartPolicy. */
    default Root root() { return null; }

    /** JSON Pointer → CEL formula. */
    default Map<String, String> compute() { return Map.of(); }
}
