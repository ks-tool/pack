package io.horchestra.pack.sdk;

import java.util.List;

/** A pack: fields are the keys of the system ({@link PackRef}), members the components it mounts. */
public interface Composition extends Component {
    List<Component> members();
}
