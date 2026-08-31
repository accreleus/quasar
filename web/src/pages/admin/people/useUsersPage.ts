// Data loading + mutation logic for UsersTab. On lib/resource (#515): a
// failed listUsers must be an error state, never an empty array that reads as
// "No users found".

import { useEffect, useState } from "react";
import * as adminApi from "../../../api/admin";
import type { AdminHome, AdminUser } from "../../../api/types";
import { useToast } from "../../../components/Toast";
import { useResource } from "../../../lib/resource/react";
import { friendlyError } from "./format";

interface UsersData {
  users: AdminUser[];
  storageByUser: Map<string, number>;
}

/** Users toolbar segment (handoff §A.18: `All` / `Admins` / `Disabled {n}`). */
export type UserSegment = "all" | "admins" | "disabled";

/** Fresh empty data each call — a module-level singleton would let one
 *  mount's (impossible, but not-worth-risking) in-place mutation leak into
 *  every other mount's "no data yet" seed. */
function emptyData(): UsersData {
  return { users: [], storageByUser: new Map() };
}

export interface UsersPageState {
  users: AdminUser[];
  loading: boolean;
  errorMessage: string | null;
  storageByUser: Map<string, number>;
  query: string;
  setQuery: (q: string) => void;
  segment: UserSegment;
  setSegment: (s: UserSegment) => void;
  disabledCount: number;
  selected: Set<string>;
  clearSelected: () => void;
  actingOn: string | null;
  bulkBusy: boolean;
  load: () => void;
  filtered: AdminUser[];
  selectedInFiltered: AdminUser[];
  allFilteredSelected: boolean;
  toggleSelect: (id: string) => void;
  toggleSelectAll: () => void;
  handleToggleRole: (u: AdminUser) => Promise<void>;
  handleToggleDisable: (u: AdminUser) => Promise<void>;
  handleDelete: (u: AdminUser) => Promise<void>;
  handleBulkDisable: () => Promise<void>;
  handleBulkDelete: () => Promise<void>;
  handleBulkRole: (role: "user" | "admin") => Promise<void>;
  replaceUser: (updated: AdminUser) => void;
}

export function useUsersPage(): UsersPageState {
  const { addToast } = useToast();

  const resource = useResource<UsersData>({
    label: "users",
    initialData: emptyData(),
    fetch: async (ctx) => {
      const { items } = await adminApi.listUsers(ctx.token);

      // Storage usage is a best-effort secondary read — silently skip (keeping
      // whatever the previous load already had) if P5-05 isn't deployed yet.
      // A users-list failure above is NOT best-effort: it throws and becomes
      // this resource's error state, which is the bug #515 fixes.
      let storageByUser = ctx.current?.storageByUser ?? new Map<string, number>();
      try {
        const { items: homes } = await adminApi.listAdminHomes(ctx.token);
        const map = new Map<string, number>();
        for (const h of homes as AdminHome[]) {
          if (h.user_id == null) continue;
          map.set(h.user_id, (map.get(h.user_id) ?? 0) + h.bytes_used);
        }
        storageByUser = map;
      } catch {
        /* endpoint not live yet */
      }

      return { users: items, storageByUser };
    },
  });

  const users = resource.data?.users ?? [];
  const storageByUser = resource.data?.storageByUser ?? emptyData().storageByUser;

  const [query, setQuery] = useState("");
  const [segment, setSegment] = useState<UserSegment>("all");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [actingOn, setActingOn] = useState<string | null>(null);
  const [bulkBusy, setBulkBusy] = useState(false);

  const load = () => void resource.refresh();

  // ── Filtered rows ─────────────────────────────────────────────────────────
  // Segment and search compose into one `filtered` array — selection
  // (toggleSelectAll, allFilteredSelected) reads this same array, so a
  // select-all under "Admins" can never reach into rows the segment hides.

  const disabledCount = users.filter((u) => u.disabled).length;

  const bySegment =
    segment === "admins"
      ? users.filter((u) => u.role === "admin")
      : segment === "disabled"
        ? users.filter((u) => u.disabled)
        : users;

  const filtered = query.trim()
    ? bySegment.filter((u) => {
        const q = query.toLowerCase();
        return u.username.toLowerCase().includes(q) || u.email.toLowerCase().includes(q);
      })
    : bySegment;

  // A segment/search change can hide a previously-selected row. Trim the
  // selection down to the new `filtered` set (rather than wiping it) so a
  // still-visible selected row stays selected — the bulk bar, the confirm
  // modal's count, and the requests a bulk action sends all read `filtered`
  // intersected with `selected`, so this keeps all three in agreement.
  useEffect(() => {
    setSelected((prev) => {
      const visible = new Set(filtered.map((u) => u.id));
      const next = new Set([...prev].filter((id) => visible.has(id)));
      return next.size === prev.size ? prev : next;
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [segment, query]);

  const selectedInFiltered = filtered.filter((u) => selected.has(u.id));

  // ── Selection helpers ─────────────────────────────────────────────────────

  const toggleSelect = (id: string) => {
    setSelected((prev) => {
      const s = new Set(prev);
      s.has(id) ? s.delete(id) : s.add(id);
      return s;
    });
  };

  const allFilteredSelected =
    filtered.length > 0 && filtered.every((u) => selected.has(u.id));

  const toggleSelectAll = () => {
    if (allFilteredSelected) {
      setSelected((prev) => {
        const s = new Set(prev);
        filtered.forEach((u) => s.delete(u.id));
        return s;
      });
    } else {
      setSelected((prev) => {
        const s = new Set(prev);
        filtered.forEach((u) => s.add(u.id));
        return s;
      });
    }
  };

  // ── Action handlers ───────────────────────────────────────────────────────
  // Writes route through resource.mutate(): its rejection goes to the CALLER
  // (these catch blocks), never into resource state — load errors and write
  // errors are deliberately different surfaces (lib/resource/core.ts).

  const handleToggleRole = async (u: AdminUser) => {
    setActingOn(u.id);
    const newRole = u.role === "admin" ? "user" : "admin";
    try {
      const { user } = await resource.mutate(
        (ctx) => adminApi.updateUser(ctx.token, u.id, { role: newRole }),
        (data, result) => ({
          ...data,
          users: data.users.map((x) => (x.id === result.user.id ? result.user : x)),
        }),
      );
      addToast({ variant: "success", title: `${user.username} is now ${newRole}` });
    } catch (e: unknown) {
      addToast({ variant: "danger", title: "Role change failed", body: friendlyError(e) });
    } finally {
      setActingOn(null);
    }
  };

  const handleToggleDisable = async (u: AdminUser) => {
    setActingOn(u.id);
    try {
      const { user } = await resource.mutate(
        (ctx) => adminApi.updateUser(ctx.token, u.id, { disabled: !u.disabled }),
        (data, result) => ({
          ...data,
          users: data.users.map((x) => (x.id === result.user.id ? result.user : x)),
        }),
      );
      addToast({
        variant: "success",
        title: `${user.username} ${user.disabled ? "disabled" : "enabled"}`,
      });
    } catch (e: unknown) {
      addToast({ variant: "danger", title: "Status change failed", body: friendlyError(e) });
    } finally {
      setActingOn(null);
    }
  };

  const handleDelete = async (u: AdminUser) => {
    setActingOn(u.id);
    try {
      await resource.mutate(
        (ctx) => adminApi.deleteUser(ctx.token, u.id),
        (data) => ({ ...data, users: data.users.filter((x) => x.id !== u.id) }),
      );
      setSelected((prev) => {
        const s = new Set(prev);
        s.delete(u.id);
        return s;
      });
      addToast({ variant: "success", title: `${u.username} deleted` });
    } catch (e: unknown) {
      addToast({ variant: "danger", title: "Delete failed", body: friendlyError(e) });
    } finally {
      setActingOn(null);
    }
  };

  // One request per id in `ids` — callers pass `selectedInFiltered`'s ids
  // (never raw `selected`), so a row hidden by the current segment/search can
  // never be acted on even if it is still technically "selected". `verb`
  // reads into the success toast ("3 users {verb}"); `failureHint` reads into
  // the failure toast's explanation of the ones that didn't make it.
  const runBulk = async (
    ids: string[],
    fn: (id: string) => Promise<unknown>,
    verb: string,
    failureHint = "refused by server",
  ) => {
    if (ids.length === 0) return;
    setBulkBusy(true);
    let ok = 0;
    let fail = 0;
    for (const id of ids) {
      try {
        await fn(id);
        ok++;
      } catch {
        fail++;
      }
    }
    setBulkBusy(false);
    setSelected(new Set());
    if (fail === 0) {
      addToast({ variant: "success", title: `${ok} user${ok === 1 ? "" : "s"} ${verb}` });
    } else {
      addToast({
        variant: "danger",
        title: `${fail} failed`,
        body: `${ok} succeeded, ${fail} ${failureHint}`,
      });
    }
  };

  const handleBulkDisable = () =>
    runBulk(
      selectedInFiltered.map((u) => u.id),
      (id) =>
        resource.mutate(
          (ctx) => adminApi.updateUser(ctx.token, id, { disabled: true }),
          (data, result) => ({
            ...data,
            users: data.users.map((x) => (x.id === result.user.id ? result.user : x)),
          }),
        ),
      "disabled",
    );

  const handleBulkRole = (role: "user" | "admin") =>
    runBulk(
      selectedInFiltered.map((u) => u.id),
      (id) =>
        resource.mutate(
          (ctx) => adminApi.updateUser(ctx.token, id, { role }),
          (data, result) => ({
            ...data,
            users: data.users.map((x) => (x.id === result.user.id ? result.user : x)),
          }),
        ),
      `set to ${role}`,
    );

  const handleBulkDelete = () =>
    runBulk(
      selectedInFiltered.map((u) => u.id),
      (id) =>
        resource.mutate(
          (ctx) => adminApi.deleteUser(ctx.token, id),
          (data) => ({ ...data, users: data.users.filter((x) => x.id !== id) }),
        ),
      "deleted",
      "refused (last admin / active sessions)",
    );

  const clearSelected = () => setSelected(new Set());
  const replaceUser = (updated: AdminUser) =>
    resource.setData((data) => ({
      ...data,
      users: data.users.map((u) => (u.id === updated.id ? updated : u)),
    }));

  return {
    users,
    loading: resource.loading,
    errorMessage: resource.errorMessage,
    storageByUser,
    query,
    setQuery,
    segment,
    setSegment,
    disabledCount,
    selected,
    clearSelected,
    actingOn,
    bulkBusy,
    load,
    filtered,
    selectedInFiltered,
    allFilteredSelected,
    toggleSelect,
    toggleSelectAll,
    handleToggleRole,
    handleToggleDisable,
    handleDelete,
    handleBulkDisable,
    handleBulkDelete,
    handleBulkRole,
    replaceUser,
  };
}
