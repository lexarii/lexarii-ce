// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Checkbox
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.produceState
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.lifecycle.viewmodel.compose.viewModel
import com.lexarii.app.api.LibraryItem
import com.lexarii.app.reader.PdfReaderScreen
import com.lexarii.app.reader.ReaderScreen
import kotlinx.coroutines.flow.distinctUntilChanged

/**
 * Sign in, the shelf, and the reader over it.
 *
 * The shelf is the server's when it answers and the one saved from the last
 * run when it does not, and a book kept on the device opens either way.
 */
class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // Android 15 draws every app targeting it edge to edge whether it asks
        // or not. Asking is what gets the status bar icons a colour that can be
        // seen against the theme; the padding below keeps content out from under
        // them.
        enableEdgeToEdge()
        setContent {
            LexariiTheme {
                Surface(modifier = Modifier.fillMaxSize()) {
                    LexariiApp()
                }
            }
        }
    }
}

@Composable
fun LexariiApp(model: LibraryViewModel = viewModel(factory = LibraryViewModel.Factory)) {
    val state by model.state.collectAsState()
    val reading by model.reading.collectAsState()
    val settings by model.readingSettings.collectAsState()

    when (val open = reading) {
        is Reading.Epub -> {
            ReaderScreen(open.controller, open.server, settings, onSettings = model::saveReadingSettings, onClose = { model.closeReader() })
            return
        }
        is Reading.Pdf -> {
            PdfReaderScreen(open.controller, settings, onSettings = model::saveReadingSettings, onClose = { model.closeReader() })
            return
        }
        is Reading.Opening, is Reading.CannotOpen -> {
            BackHandler { model.closeReader() }
            Column(
                modifier = Modifier.fillMaxSize().safeDrawingPadding().padding(24.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp, Alignment.CenterVertically),
                horizontalAlignment = Alignment.CenterHorizontally,
            ) {
                if (open is Reading.CannotOpen) {
                    Text(open.message, style = MaterialTheme.typography.bodyLarge)
                    TextButton(onClick = { model.closeReader() }) { Text("Back to the library") }
                } else {
                    Text("Opening…", style = MaterialTheme.typography.bodyLarge)
                }
            }
            return
        }
        null -> Unit
    }

    Column(
        // safeDrawing covers the status bar, the navigation bar, a display
        // cutout, and the keyboard, so the password field is never behind it.
        modifier = Modifier.fillMaxSize().safeDrawingPadding().padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        when (val current = state) {
            is UiState.SignedOut -> SignIn(current, model)
            is UiState.Loading -> Text("Loading…", style = MaterialTheme.typography.bodyLarge)
            is UiState.SigningOut -> Text("Signing out…", style = MaterialTheme.typography.bodyLarge)
            is UiState.Unreachable -> Unreachable(current, model)
            is UiState.Library -> Shelf(current, model)
        }
    }

    val cost by model.confirmSignOut.collectAsState()
    cost?.let { SignOutDialog(it, model) }
}

@Composable
private fun SignOutDialog(cost: SignOutCost, model: LibraryViewModel) {
    val lost = buildList {
        if (cost.kept > 0) add(if (cost.kept == 1) "the book kept on this device" else "the ${cost.kept} books kept on this device")
        if (cost.unsent > 0) {
            add(if (cost.unsent == 1) "a reading position not yet sent" else "${cost.unsent} reading positions not yet sent")
        }
        if (cost.unsentMarks > 0) {
            add(
                if (cost.unsentMarks == 1) "a highlight or note not yet sent"
                else "${cost.unsentMarks} highlights and notes not yet sent",
            )
        }
        if (cost.refusedMarks > 0) {
            add(
                if (cost.refusedMarks == 1) "a highlight or note the server would not accept, which is kept only here"
                else "${cost.refusedMarks} highlights and notes the server would not accept, which are kept only here",
            )
        }
    }
    AlertDialog(
        onDismissRequest = { model.cancelSignOut() },
        title = { Text("Sign out?") },
        text = {
            val listed = if (lost.size > 1) lost.dropLast(1).joinToString(", ") + " and " + lost.last() else lost.joinToString()
            Text("This removes $listed. Books can be kept again after signing back in.")
        },
        confirmButton = { TextButton(onClick = { model.signOut() }) { Text("Sign out") } },
        dismissButton = { TextButton(onClick = { model.cancelSignOut() }) { Text("Stay signed in") } },
    )
}

@Composable
private fun Unreachable(state: UiState.Unreachable, model: LibraryViewModel) {
    Text("Can't reach your library", style = MaterialTheme.typography.headlineSmall)
    Text(state.server, style = MaterialTheme.typography.bodyMedium)
    Text(state.error, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium)
    Button(onClick = { model.retry() }, modifier = Modifier.fillMaxWidth()) {
        Text("Try again")
    }
    // The way out when the server has moved or the token was revoked while
    // this device was away - otherwise retrying is all anybody could do.
    TextButton(onClick = { model.requestSignOut() }, modifier = Modifier.fillMaxWidth()) {
        Text("Sign in again")
    }
}

@Composable
private fun SignIn(state: UiState.SignedOut, model: LibraryViewModel) {
    var server by remember { mutableStateOf(state.server) }
    var username by remember { mutableStateOf(state.username) }
    var password by remember { mutableStateOf("") }

    Text("Lexarii", style = MaterialTheme.typography.headlineMedium)
    OutlinedTextField(
        value = server,
        onValueChange = { server = it },
        label = { Text("Server") },
        // Plain http in the example, deliberately: a self-hosted server on a
        // home network usually has no certificate, and the app allows
        // cleartext for that reason (res/xml/network_security_config.xml).
        placeholder = { Text("http://books.local:8080") },
        modifier = Modifier.fillMaxWidth(),
    )
    OutlinedTextField(
        value = username,
        onValueChange = { username = it },
        label = { Text("Username") },
        modifier = Modifier.fillMaxWidth(),
    )
    OutlinedTextField(
        value = password,
        onValueChange = { password = it },
        label = { Text("Password") },
        visualTransformation = PasswordVisualTransformation(),
        modifier = Modifier.fillMaxWidth(),
    )
    Button(
        onClick = { model.signIn(server, username, password) },
        modifier = Modifier.fillMaxWidth(),
    ) {
        Text("Sign in")
    }
    state.error?.let {
        Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium)
    }
}

/**
 * Keep, asking first - once, and only then - for leave to show the download's
 * progress as a notification. It is asked when a download starts because that
 * is when it means something; refused, the download runs all the same.
 */
@Composable
private fun rememberKeep(model: LibraryViewModel): (LibraryItem) -> Unit {
    val context = LocalContext.current
    var asked by rememberSaveable { mutableStateOf(false) }
    var waiting by remember { mutableStateOf<LibraryItem?>(null) }
    val ask = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) {
        waiting?.let(model::keep)
        waiting = null
    }
    return { item ->
        val needed = Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        if (needed && !asked) {
            asked = true
            waiting = item
            ask.launch(Manifest.permission.POST_NOTIFICATIONS)
        } else {
            model.keep(item)
        }
    }
}

@Composable
private fun Shelf(state: UiState.Library, model: LibraryViewModel) {
    val keeping by model.keeping.collectAsState()
    val unmeteredOnly by model.unmeteredOnly.collectAsState()
    val keep = rememberKeep(model)
    var menu by remember { mutableStateOf(false) }

    Row(verticalAlignment = Alignment.CenterVertically) {
        val count = state.items.size
        Text(
            if (state.complete) "$count books" else "$count books so far…",
            style = MaterialTheme.typography.titleMedium,
            modifier = Modifier.weight(1f),
        )
        Box {
            TextButton(onClick = { menu = true }) { Text("⋮") }
            DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                DropdownMenuItem(
                    text = { Text("Keep books over Wi-Fi only") },
                    leadingIcon = { Checkbox(checked = unmeteredOnly, onCheckedChange = null) },
                    onClick = { model.setUnmeteredOnly(!unmeteredOnly) },
                )
                DropdownMenuItem(
                    text = { Text("Sign out") },
                    onClick = {
                        menu = false
                        model.requestSignOut()
                    },
                )
            }
        }
    }
    if (state.offline != null) {
        // Said once, at the top, rather than as a greyed-out list with no
        // explanation for why most of it will not open.
        Text(
            "Offline - only books kept on this device will open. (${state.offline})",
            color = MaterialTheme.colorScheme.error,
            style = MaterialTheme.typography.bodySmall,
        )
        TextButton(onClick = { model.retry() }) { Text("Try again") }
    } else {
        state.syncedAt?.let {
            Text("Synced $it", style = MaterialTheme.typography.bodySmall)
        }
    }

    val list = rememberLazyListState()
    // Near the end of what has arrived, the rest is asked for - if it is not
    // already on its way behind the first page.
    LaunchedEffect(list, state.items.size) {
        snapshotFlow { list.layoutInfo.visibleItemsInfo.lastOrNull()?.index ?: 0 }
            .distinctUntilChanged()
            .collect { last -> if (last >= state.items.size - PREFETCH_ROWS) model.loadMore() }
    }
    LazyColumn(state = list, verticalArrangement = Arrangement.spacedBy(4.dp)) {
        items(state.items, key = { it.bookFileId }) { item ->
            val copy = keeping[item.bookFileId]
            ShelfRow(
                item = item,
                copy = copy,
                model = model,
                // Offline, a book opens only from its kept copy.
                openable = !item.missing && (state.offline == null || copy == Keeping.Kept),
                onOpen = { model.open(item) },
                onKeep = { keep(item) },
                onDiscard = { model.discard(item.bookFileId) },
                // Removing a copy with no network to fetch it again would
                // leave nothing to read, one mis-tap from the reason it was kept.
                canDiscard = state.offline == null,
                // Nor can one be fetched: the button would only fail.
                canKeep = state.offline == null,
            )
        }
        if (state.offline == null && !state.complete) {
            item(key = "more") {
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp),
                ) {
                    if (state.failed != null && !state.loadingMore) {
                        Text(
                            "The rest of the library did not load. (${state.failed})",
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.error,
                            modifier = Modifier.weight(1f),
                        )
                        TextButton(onClick = { model.loadMore() }) { Text("Try again") }
                    } else {
                        Text("Loading more…", style = MaterialTheme.typography.bodySmall)
                    }
                }
            }
        }
    }
}

// How close to the end of the list the next page is asked for.
private const val PREFETCH_ROWS = 15

@Composable
private fun ShelfRow(
    item: LibraryItem,
    copy: Keeping?,
    model: LibraryViewModel,
    openable: Boolean,
    onOpen: () -> Unit,
    onKeep: () -> Unit,
    onDiscard: () -> Unit,
    canDiscard: Boolean,
    canKeep: Boolean,
) {
    Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.fillMaxWidth()) {
        // The whole row is the target, and at least 48dp of it: a title is a
        // small thing to hit with a thumb.
        Row(
            verticalAlignment = Alignment.CenterVertically,
            modifier = Modifier
                .weight(1f)
                .clickable(enabled = openable, onClick = onOpen)
                .heightIn(min = 48.dp)
                .padding(vertical = 6.dp),
        ) {
            Cover(item, model)
            Spacer(Modifier.width(12.dp))
            Column {
                val dim = if (openable) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.onSurfaceVariant
                Text(item.title, style = MaterialTheme.typography.bodyLarge, color = dim, maxLines = 3, overflow = TextOverflow.Ellipsis)
                val by = item.authors.joinToString(", ")
                if (by.isNotBlank()) {
                    Text(by, style = MaterialTheme.typography.bodySmall, color = dim, maxLines = 2, overflow = TextOverflow.Ellipsis)
                }
                if (copy is Keeping.Failed) {
                    Text(copy.message, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
                }
            }
        }
        // Only a book the owner lets this person download is kept: a book
        // lent to be read is not quietly kept.
        val keepable = item.formats.map { it.lowercase() }.any { it == "epub" || it == "pdf" }
        if (item.canDownload && keepable) {
            when (copy) {
                null, is Keeping.Failed -> if (canKeep) TextButton(onClick = onKeep) { Text("Keep") }
                Keeping.Waiting -> TextButton(onClick = onDiscard) { Text("Waiting ✕") }
                is Keeping.Downloading -> TextButton(onClick = onDiscard) {
                    Text(
                        when {
                            copy.total <= 0 -> "… ✕"
                            copy.inBytes -> "${copy.done * 100 / copy.total}% ✕"
                            else -> "${copy.done}/${copy.total} ✕"
                        },
                    )
                }
                Keeping.Kept -> TextButton(onClick = onDiscard, enabled = canDiscard) { Text("Kept ✓") }
            }
        }
    }
}

/** The cover, or a plain block the same size until there is one. */
@Composable
private fun Cover(item: LibraryItem, model: LibraryViewModel) {
    val shape = RoundedCornerShape(4.dp)
    val modifier = Modifier.size(width = 48.dp, height = 72.dp).clip(shape)
    val image by produceState<ImageBitmap?>(model.covers.cached(item.bookFileId), item.bookFileId, item.coverUrl) {
        if (value == null && item.coverUrl != null) value = model.cover(item.bookFileId)
    }
    val shown = image
    if (shown != null) {
        Image(shown, contentDescription = null, contentScale = ContentScale.Crop, modifier = modifier)
    } else {
        Box(modifier.background(MaterialTheme.colorScheme.surfaceVariant))
    }
}
