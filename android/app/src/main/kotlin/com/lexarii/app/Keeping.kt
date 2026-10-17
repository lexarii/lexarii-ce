// SPDX-License-Identifier: AGPL-3.0-or-later

package com.lexarii.app

import android.app.NotificationChannel
import android.app.NotificationManager
import android.content.Context
import android.content.pm.ServiceInfo
import android.os.Build
import androidx.core.app.NotificationCompat
import androidx.work.BackoffPolicy
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingWorkPolicy
import androidx.work.ForegroundInfo
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.WorkInfo
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import androidx.work.workDataOf
import com.lexarii.app.api.DataStoreTokenStore
import com.lexarii.app.api.LexariiClient
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import java.util.UUID
import java.util.concurrent.TimeUnit

/** A BookFile's copy on this device. */
sealed interface Keeping {
    /** Asked for, and waiting on a network - or on Wi-Fi, if that is the setting - or on a retry. */
    data object Waiting : Keeping

    /** `inBytes`: a PDF counts bytes of its file; an EPUB counts chapters. */
    data class Downloading(val done: Long, val total: Long, val inBytes: Boolean) : Keeping
    data object Kept : Keeping
    data class Failed(val message: String) : Keeping
}

/**
 * Keeping books on the device, as work Android runs and resumes.
 *
 * WorkManager rather than a coroutine in the screen's scope: a download of a
 * large PDF outlives the screen that asked for it, and Android stops apps in
 * the background. The work is persisted by the system, run again when the
 * process comes back or the network does, and every run carries on from the
 * partial copy the last one left. One unique piece of work per BookFile, so
 * asking twice keeps once.
 */
class KeepQueue(context: Context) {
    private val work = WorkManager.getInstance(context.applicationContext)

    /** Every book being kept or waiting to be, by BookFile, and the ones that failed. */
    val states: Flow<Map<String, Keeping>> = work.getWorkInfosByTagFlow(TAG).map { infos ->
        buildMap {
            for (info in infos) {
                val id = info.tags.tagged(ID_TAG) ?: continue
                val state = when (info.state) {
                    WorkInfo.State.ENQUEUED, WorkInfo.State.BLOCKED -> Keeping.Waiting
                    WorkInfo.State.RUNNING -> {
                        val p = info.progress
                        if (p.keyValueMap.containsKey(DONE)) {
                            Keeping.Downloading(p.getLong(DONE, 0), p.getLong(TOTAL, 0), p.getBoolean(BYTES, false))
                        } else {
                            Keeping.Downloading(0, 0, false)
                        }
                    }
                    WorkInfo.State.SUCCEEDED -> Keeping.Kept
                    WorkInfo.State.FAILED -> Keeping.Failed(info.outputData.getString(MESSAGE) ?: "The download failed")
                    WorkInfo.State.CANCELLED -> null
                } ?: continue
                // The newest word on a book wins over an older one still listed.
                val existing = this[id]
                if (existing == null || existing is Keeping.Failed || existing == Keeping.Kept) put(id, state)
            }
        }
    }

    fun keep(bookFileId: String, title: String, account: String, unmeteredOnly: Boolean) {
        // KEEP: a download already queued or running for this book carries on.
        work.enqueueUniqueWork(UNIQUE + bookFileId, ExistingWorkPolicy.KEEP, request(bookFileId, title, account, unmeteredOnly))
    }

    /**
     * The Wi-Fi setting changed: every download not yet finished takes it on,
     * not only the ones asked for from now. Updated in place, under the same
     * id - a download running now carries on and the next run obeys it, and
     * one waiting for Wi-Fi starts as soon as any network will do.
     */
    suspend fun retarget(account: String, unmeteredOnly: Boolean) {
        for (info in work.getWorkInfosByTagFlow(TAG).first()) {
            if (info.state.isFinished) continue
            val id = info.tags.tagged(ID_TAG) ?: continue
            val title = info.tags.tagged(TITLE_TAG).orEmpty()
            runCatching { work.updateWork(request(id, title, account, unmeteredOnly, info.id)) }
        }
    }

    private fun request(bookFileId: String, title: String, account: String, unmeteredOnly: Boolean, id: UUID? = null) =
        OneTimeWorkRequestBuilder<KeepWorker>()
            .apply { if (id != null) setId(id) }
            .setInputData(workDataOf(KEY_ID to bookFileId, KEY_TITLE to title, KEY_ACCOUNT to account))
            .setConstraints(
                Constraints.Builder()
                    .setRequiredNetworkType(if (unmeteredOnly) NetworkType.UNMETERED else NetworkType.CONNECTED)
                    .build(),
            )
            .setBackoffCriteria(BackoffPolicy.EXPONENTIAL, 30, TimeUnit.SECONDS)
            .addTag(TAG)
            .addTag(ID_TAG + bookFileId)
            // So the request can be built again, whole, when the setting changes.
            .addTag(TITLE_TAG + title)
            .build()

    private fun Set<String>.tagged(prefix: String): String? =
        firstNotNullOfOrNull { tag -> tag.takeIf { it.startsWith(prefix) }?.removePrefix(prefix) }

    fun cancel(bookFileId: String) {
        work.cancelUniqueWork(UNIQUE + bookFileId)
    }

    /** Signing out: nothing of this account's may still be fetching, or be fetched later. */
    fun cancelAll() {
        work.cancelAllWorkByTag(TAG)
        work.pruneWork()
    }

    companion object {
        const val TAG = "keep"
        private const val ID_TAG = "keep:"
        private const val TITLE_TAG = "keep-title:"
        private const val UNIQUE = "keep-"
        internal const val KEY_ID = "bookFileId"
        internal const val KEY_TITLE = "title"
        internal const val KEY_ACCOUNT = "account"
        internal const val DONE = "done"
        internal const val TOTAL = "total"
        internal const val BYTES = "bytes"
        internal const val MESSAGE = "message"
    }
}

/**
 * One book being kept. Runs in the background, as a foreground service with
 * a progress notification when Android allows one; carries on from wherever
 * the last run stopped.
 */
class KeepWorker(context: Context, params: WorkerParameters) : CoroutineWorker(context, params) {
    override suspend fun doWork(): Result {
        val id = inputData.getString(KeepQueue.KEY_ID) ?: return failed("Nothing to keep")
        val account = inputData.getString(KeepQueue.KEY_ACCOUNT) ?: return failed("Nothing to keep")
        val title = inputData.getString(KeepQueue.KEY_TITLE).orEmpty()
        val job = KeepJob(
            device = Services.device(applicationContext),
            tokens = DataStoreTokenStore(applicationContext),
            clientFor = { server, tokens -> LexariiClient(server, tokens) },
        )
        val note = KeepNotification(applicationContext, id, title)
        val progress = object : KeepProgress {
            override suspend fun foreground(): Boolean = try {
                // A foreground service keeps a long download going with the
                // screen off. Android refuses one to an app that is not in
                // front at the moment the work starts - Wi-Fi arriving, a
                // retry coming due - and then the work runs as ordinary
                // background work: Android may stop it after some minutes,
                // and the next run resumes.
                setForeground(note.foreground(0, 0, false))
                true
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                false
            }

            override fun update(done: Long, total: Long, inBytes: Boolean) {
                setProgressAsync(workDataOf(KeepQueue.DONE to done, KeepQueue.TOTAL to total, KeepQueue.BYTES to inBytes))
                note.update(done, total, inBytes)
            }

            override fun finish() = note.finish()
        }
        return when (val outcome = job.run(id, account, runAttemptCount, progress)) {
            KeepJob.Outcome.Kept -> Result.success()
            KeepJob.Outcome.Retry -> Result.retry()
            is KeepJob.Outcome.Failed -> failed(outcome.message)
        }
    }

    private fun failed(message: String) = Result.failure(workDataOf(KeepQueue.MESSAGE to message))
}

/** The notification a download shows while it runs. Silent, and gone when it ends. */
private class KeepNotification(private val context: Context, bookFileId: String, private val title: String) {
    private val id = NOTIFICATION_BASE + (bookFileId.hashCode() and 0xffff)
    private val manager = context.getSystemService(NotificationManager::class.java)

    init {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            manager.createNotificationChannel(
                NotificationChannel(CHANNEL, "Keeping books on this device", NotificationManager.IMPORTANCE_LOW),
            )
        }
    }

    fun foreground(done: Long, total: Long, bytes: Boolean): ForegroundInfo {
        val notification = build(done, total, bytes)
        return if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            ForegroundInfo(id, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
        } else {
            ForegroundInfo(id, notification)
        }
    }

    // Set once the run is over. A progress tick arriving after it - the last
    // one of a cancelled run - must not post the notification again.
    @Volatile
    private var finished = false

    /**
     * Updates the same notification. Without the permission to post it on
     * 13 and later this does nothing visible, and the download goes on.
     */
    fun update(done: Long, total: Long, bytes: Boolean) {
        if (finished) return
        runCatching { manager.notify(id, build(done, total, bytes)) }
    }

    /**
     * Takes the notification away. A run Android refused a foreground service
     * posted it itself, and nothing else would ever remove it - it would sit
     * there at 37% after the download finished, failed, or its account signed
     * out. While it is a foreground service's own, the system ignores this and
     * WorkManager removes it when the service stops.
     */
    fun finish() {
        finished = true
        runCatching { manager.cancel(id) }
    }

    private fun build(done: Long, total: Long, bytes: Boolean) =
        NotificationCompat.Builder(context, CHANNEL)
            .setSmallIcon(android.R.drawable.stat_sys_download)
            .setContentTitle(if (title.isNotBlank()) "Keeping “$title”" else "Keeping a book")
            .setContentText(
                when {
                    total <= 0 -> "Starting…"
                    bytes -> "${done * 100 / total}%"
                    else -> "$done of $total chapters"
                },
            )
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setSilent(true)
            .setProgress(
                if (total > 0) 1000 else 0,
                if (total > 0) (done * 1000 / total).toInt() else 0,
                total <= 0,
            )
            .build()

    private companion object {
        const val CHANNEL = "keeping"
        const val NOTIFICATION_BASE = 0x5300000
    }
}
