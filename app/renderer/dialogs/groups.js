"use strict";

// Create and edit group dialog.
// Loaded in the order declared by renderer/index.html.


let editingGroup = "group";
let groupMemberDraft = {};

function openGroupModal(id) {
  editingGroup = id || "group";
  const isDefault = editingGroup === "group";
  const g = isDefault
    ? { title: settingsOf().group_title || "", avatar: settingsOf().group_avatar || "", members: state.group_members || [] }
    : (groupOf(editingGroup) || { title: "", avatar: "", members: [] });
  const form = $("groupForm");
  form.reset();
  $("groupErr").textContent = "";
  $("groupDeleteBtn").hidden = isDefault;
  fld(form, "group_title").value = g.title || "";
  avatarDraft.group = g.avatar || "";
  paintAvatarEditor("group", editingGroup);
  groupMemberDraft[editingGroup] = {};
  for (const n of g.members) groupMemberDraft[editingGroup][n] = true;
  renderGroupMembers(editingGroup);
  $("groupModal").showModal();
}
$("groupForm").addEventListener("submit", async (e) => {
  if (e.submitter && e.submitter.value === "cancel") return;
  e.preventDefault();
  const title = fld($("groupForm"), "group_title").value.trim();
  const avatar = avatarDraft.group;
  const members = state.bots.map((b) => b.name).filter((n) => groupMemberDraft[editingGroup][n]);
  try {
    if (editingGroup === "group") {
      await api("/api/settings", { group_title: title, group_avatar: avatar });

      // Submit only the members that changed: adding one bot must not run everyone else through a join
      const was = new Set(state.group_members || []);
      for (const b of state.bots) {
        const now = !!groupMemberDraft[editingGroup][b.name];
        if (now !== was.has(b.name)) await api("/api/group/set", { group: "group", name: b.name, in: now });
      }
    } else {
      await api("/api/groups/update", { id: editingGroup, title, avatar, members });
    }
    $("groupModal").close();
  } catch (err) {
    $("groupErr").textContent = err.message;
  }
});
$("groupDeleteBtn").onclick = async () => {
  const ok = await ask({
    title: t("Delete this group"),
    hint: t("Messages in this group are kept."),
    ok: t("Delete"),
    danger: true,
  });
  if (!ok) return;
  try {
    await api("/api/groups/delete", { id: editingGroup });
    $("groupModal").close();
    if (current === editingGroup) switchChat("group");
  } catch (err) {
    $("groupErr").textContent = err.message;
  }
};

// Creating a group: persist an empty one to get its id, then open its settings so the user can name it
// and add members. One step fewer than a create-form first, and an abandoned empty group can be
// deleted right there in the same dialog.
async function createGroup() {
  const g = await api("/api/groups", { title: "", avatar: "", members: [] }).catch((err) => toast(err.message));
  if (g && g.id) openGroupModal(g.id);
}

$("tokenForm").addEventListener("submit", (e) => {
  if (e.submitter && e.submitter.value === "cancel") return;
  e.preventDefault();
  const v = String(new FormData($("tokenForm")).get("tvalue") || "").trim();
  if (!v) return;
  TOKEN = v;
  localStorage.setItem("botbureau_token:" + BACKEND, v);
  $("tokenModal").close();
  boot();
});
