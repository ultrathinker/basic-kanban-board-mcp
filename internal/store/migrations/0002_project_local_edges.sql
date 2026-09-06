-- 0002_project_local_edges — keep hierarchy and dependency edges inside one project.
--
-- Foreign keys preserve existence, not locality: 0001 guarantees that a
-- parent task and both ends of a link exist, and says nothing about which
-- project they belong to. That gap is what lets a task visible to a
-- project-scoped token be blocked by, or parented under, a task the token
-- cannot read — the blocker key then leaks into compact output and
-- task_next refuses work for a reason the caller cannot see.
--
-- The service layer rejects cross-project edges on the way in. This file is
-- the backstop underneath it, for the paths that do not go through the
-- service layer: an import, an admin CLI, a future migration, or a repair
-- someone runs by hand against the file.
--
-- Triggers rather than composite foreign keys. A composite FK would need
-- UNIQUE(id, project_id) on tasks plus a rewrite of both `tasks` and
-- `links` — SQLite cannot add a constraint in place, so it means recreating
-- a table that references itself, carries fourteen indexes, and is the
-- target of three other tables' foreign keys. Triggers express exactly the
-- same rule, fire on every INSERT and UPDATE the same way a constraint
-- would, and cost one migration file instead of a table rebuild.
--
-- Every WHEN clause is written so it fires only when both rows exist. A
-- missing row is a foreign-key violation and must keep reporting itself as
-- one: "task X does not exist" is an error the caller can act on, and
-- "cross-project edge" in its place would send them looking for the wrong
-- problem.

-- A link may only connect two tasks in the same project.
CREATE TRIGGER links_project_local_insert
BEFORE INSERT ON links
FOR EACH ROW
WHEN EXISTS (
    SELECT 1 FROM tasks blocker, tasks blocked
     WHERE blocker.id = NEW.blocker_id
       AND blocked.id = NEW.blocked_id
       AND blocker.project_id <> blocked.project_id
)
BEGIN
    SELECT RAISE(ABORT, 'kanban: cross-project edge (a link must connect two tasks in the same project)');
END;

CREATE TRIGGER links_project_local_update
BEFORE UPDATE ON links
FOR EACH ROW
WHEN EXISTS (
    SELECT 1 FROM tasks blocker, tasks blocked
     WHERE blocker.id = NEW.blocker_id
       AND blocked.id = NEW.blocked_id
       AND blocker.project_id <> blocked.project_id
)
BEGIN
    SELECT RAISE(ABORT, 'kanban: cross-project edge (a link must connect two tasks in the same project)');
END;

-- A subtask must live in its parent's project.
CREATE TRIGGER tasks_parent_project_local_insert
BEFORE INSERT ON tasks
FOR EACH ROW
WHEN NEW.parent_id IS NOT NULL
 AND EXISTS (
    SELECT 1 FROM tasks parent
     WHERE parent.id = NEW.parent_id
       AND parent.project_id <> NEW.project_id
)
BEGIN
    SELECT RAISE(ABORT, 'kanban: cross-project edge (a subtask must live in its parent''s project)');
END;

-- The same rule on update, plus the mirror case: moving a task to another
-- project would strand the edges it already has, so it is refused while it
-- still has children or links pointing outside the destination.
CREATE TRIGGER tasks_project_local_update
BEFORE UPDATE ON tasks
FOR EACH ROW
WHEN (NEW.parent_id IS NOT NULL
      AND EXISTS (
          SELECT 1 FROM tasks parent
           WHERE parent.id = NEW.parent_id
             AND parent.project_id <> NEW.project_id))
  OR (NEW.project_id <> OLD.project_id
      AND (EXISTS (
              SELECT 1 FROM tasks child
               WHERE child.parent_id = OLD.id
                 AND child.project_id <> NEW.project_id)
        OR EXISTS (
              SELECT 1 FROM links l
               WHERE (l.blocker_id = OLD.id OR l.blocked_id = OLD.id)
                 AND EXISTS (
                     SELECT 1 FROM tasks other
                      WHERE other.id = CASE WHEN l.blocker_id = OLD.id
                                            THEN l.blocked_id ELSE l.blocker_id END
                        AND other.project_id <> NEW.project_id))))
BEGIN
    SELECT RAISE(ABORT, 'kanban: cross-project edge (hierarchy and dependency edges must stay inside one project)');
END;
